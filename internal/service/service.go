package service

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const defaultProbeInterval = 300

type Account struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	BaseURL         string    `json:"base_url"`
	PublicModel     string    `json:"public_model"`
	UpstreamModel   string    `json:"upstream_model"`
	Enabled         bool      `json:"enabled"`
	Priority        int       `json:"priority"`
	APIKeySet       bool      `json:"api_key_set"`
	TTFTEWMA        float64   `json:"ttft_ewma_ms"`
	TotalEWMA       float64   `json:"total_ewma_ms"`
	Samples         int64     `json:"samples"`
	Successes       int64     `json:"successes"`
	Failures        int64     `json:"failures"`
	ConsecutiveFail int       `json:"consecutive_failures"`
	CooldownUntil   time.Time `json:"cooldown_until,omitempty"`
	LastError       string    `json:"last_error,omitempty"`
	apiKey          string
}

type Pool struct {
	PublicModel       string `json:"public_model"`
	Strategy          string `json:"strategy"`
	TimeoutMS         int    `json:"timeout_ms"`
	RequestTimeoutMS  int    `json:"request_timeout_ms"`
	MaxAttempts       int    `json:"max_attempts"`
	FailureThreshold  int    `json:"failure_threshold"`
	CooldownSeconds   int    `json:"cooldown_seconds"`
	ProbeIntervalSecs int    `json:"probe_interval_seconds"`
}

type requestEvent struct {
	At             time.Time
	PublicModel    string
	AccountID      string
	AccountName    string
	Status         int
	TTFTMS         float64
	TotalMS        float64
	Attempts       int
	ErrorType      string
	ErrorMessage   string
	Successful     bool
	Consecutive    int
	CooldownUntil  time.Time
	Probe          bool
	MetricTTFT     float64
	MetricTotal    float64
	MetricSamples  int64
	MetricSuccess  int64
	MetricFailures int64
}

type Service struct {
	db                    *sql.DB
	adminPassword         string
	clientKey             string
	aead                  cipher.AEAD
	httpClient            *http.Client
	mu                    sync.RWMutex
	activityMu            sync.Mutex
	inFlight              int
	lastProxyRequest      time.Time
	probeActive           bool
	probeCancel           context.CancelFunc
	accounts              map[string]Account
	pools                 map[string]Pool
	metricQueue           chan requestEvent
	requestQueue          chan requestEvent
	probeLast             map[string]time.Time
	probeInterval         time.Duration
	probeTimeout          time.Duration
	defaultTimeout        time.Duration
	defaultRequestTimeout time.Duration
	defaultAttempts       int
	defaultFailure        int
	defaultCooldown       int
}

type AccountInput struct {
	Name          string `json:"name"`
	BaseURL       string `json:"base_url"`
	APIKey        string `json:"api_key"`
	PublicModel   string `json:"public_model"`
	UpstreamModel string `json:"upstream_model"`
	Enabled       *bool  `json:"enabled"`
	Priority      int    `json:"priority"`
}

type PoolInput struct {
	PublicModel       string `json:"public_model"`
	Strategy          string `json:"strategy"`
	TimeoutMS         int    `json:"timeout_ms"`
	RequestTimeoutMS  int    `json:"request_timeout_ms"`
	MaxAttempts       int    `json:"max_attempts"`
	FailureThreshold  int    `json:"failure_threshold"`
	CooldownSeconds   int    `json:"cooldown_seconds"`
	ProbeIntervalSecs int    `json:"probe_interval_seconds"`
}

func New(path, adminPassword, clientKey, encryptionSecret string) (*Service, error) {
	if len(encryptionSecret) < 32 {
		return nil, errors.New("ENCRYPTION_SECRET must contain at least 32 characters")
	}
	key := sha256.Sum256([]byte(encryptionSecret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA foreign_keys=ON", "PRAGMA busy_timeout=3000"} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("sqlite pragma: %w", err)
		}
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	queueSize := envInt("METRICS_QUEUE_SIZE", 1000, 32, 100000)
	s := &Service{
		db: db, adminPassword: adminPassword, clientKey: clientKey, aead: aead,
		httpClient: &http.Client{Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment, MaxIdleConns: 128, MaxIdleConnsPerHost: 16,
			IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second,
			ExpectContinueTimeout: time.Second,
		}},
		accounts: make(map[string]Account), pools: make(map[string]Pool),
		metricQueue: make(chan requestEvent, queueSize), requestQueue: make(chan requestEvent, queueSize), probeLast: make(map[string]time.Time),
		probeInterval:         time.Duration(envInt("PROBE_INTERVAL_SECONDS", defaultProbeInterval, 0, 86400)) * time.Second,
		probeTimeout:          time.Duration(envInt("PROBE_TIMEOUT_SECONDS", 20, 1, 300)) * time.Second,
		defaultTimeout:        time.Duration(envInt("UPSTREAM_TIMEOUT_SECONDS", 120, 1, 3600)) * time.Second,
		defaultRequestTimeout: time.Duration(envInt("REQUEST_TIMEOUT_SECONDS", 600, 1, 7200)) * time.Second,
		defaultAttempts:       envInt("MAX_ATTEMPTS", 3, 1, 20), defaultFailure: envInt("FAILURE_THRESHOLD", 2, 1, 20),
		defaultCooldown: envInt("COOLDOWN_SECONDS", 60, 1, 86400),
	}
	if err := s.reload(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func migrate(db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS accounts (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, base_url TEXT NOT NULL, api_key_enc TEXT NOT NULL,
 public_model TEXT NOT NULL, upstream_model TEXT NOT NULL, enabled INTEGER NOT NULL DEFAULT 1,
 priority INTEGER NOT NULL DEFAULT 100, ttft_ewma_ms REAL NOT NULL DEFAULT 0,
 total_ewma_ms REAL NOT NULL DEFAULT 0, samples INTEGER NOT NULL DEFAULT 0,
 successes INTEGER NOT NULL DEFAULT 0, failures INTEGER NOT NULL DEFAULT 0,
 consecutive_failures INTEGER NOT NULL DEFAULT 0, cooldown_until TEXT NOT NULL DEFAULT '',
 last_error TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_accounts_model_enabled ON accounts(public_model, enabled, priority);
CREATE TABLE IF NOT EXISTS pools (
 public_model TEXT PRIMARY KEY, strategy TEXT NOT NULL DEFAULT 'fastest', timeout_ms INTEGER NOT NULL DEFAULT 120000,
 request_timeout_ms INTEGER NOT NULL DEFAULT 600000,
 max_attempts INTEGER NOT NULL DEFAULT 3, failure_threshold INTEGER NOT NULL DEFAULT 2,
 cooldown_seconds INTEGER NOT NULL DEFAULT 60, probe_interval_seconds INTEGER NOT NULL DEFAULT 300
);
CREATE TABLE IF NOT EXISTS request_events (
 id INTEGER PRIMARY KEY AUTOINCREMENT, at TEXT NOT NULL, public_model TEXT NOT NULL,
 account_id TEXT NOT NULL DEFAULT '', account_name TEXT NOT NULL DEFAULT '', status INTEGER NOT NULL DEFAULT 0,
 ttft_ms REAL NOT NULL DEFAULT 0, total_ms REAL NOT NULL DEFAULT 0, attempts INTEGER NOT NULL DEFAULT 0,
 error_type TEXT NOT NULL DEFAULT '', error_message TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_request_events_at ON request_events(at DESC);
`
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	for _, statement := range strings.Split(schema, ";") {
		statement = strings.TrimSpace(statement)
		if statement == "" {
			continue
		}
		if _, err := tx.Exec(statement); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Add columns introduced after the initial release without requiring users to
	// delete their local SQLite database.
	rows, err := db.Query(`PRAGMA table_info(pools)`)
	if err != nil {
		return err
	}
	hasRequestTimeout := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		if name == "request_timeout_ms" {
			hasRequestTimeout = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !hasRequestTimeout {
		if _, err := db.Exec(`ALTER TABLE pools ADD COLUMN request_timeout_ms INTEGER NOT NULL DEFAULT 600000`); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) Close() { _ = s.db.Close() }

func (s *Service) StartWorkers(ctx context.Context) {
	startResilientWorker(ctx, "metric writer", func() { s.metricWriter(ctx) })
	startResilientWorker(ctx, "request writer", func() { s.requestWriter(ctx) })
	startResilientWorker(ctx, "probe scheduler", func() { s.probeLoop(ctx) })
}

func (s *Service) beginProxyRequest() {
	s.activityMu.Lock()
	s.inFlight++
	s.lastProxyRequest = time.Now()
	if s.probeCancel != nil {
		s.probeCancel()
	}
	s.activityMu.Unlock()
}

func (s *Service) endProxyRequest() {
	s.activityMu.Lock()
	if s.inFlight > 0 {
		s.inFlight--
	}
	s.lastProxyRequest = time.Now()
	s.activityMu.Unlock()
}

func (s *Service) startProbe(parent context.Context, account Account) bool {
	s.activityMu.Lock()
	if s.inFlight > 0 || s.probeActive || (!s.lastProxyRequest.IsZero() && time.Since(s.lastProxyRequest) < 5*time.Second) {
		s.activityMu.Unlock()
		return false
	}
	ctx, cancel := context.WithCancel(parent)
	s.probeActive = true
	s.probeCancel = cancel
	s.activityMu.Unlock()
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("probe panic recovered", "account", account.Name, "panic", recovered)
			}
			cancel()
			s.activityMu.Lock()
			s.probeActive = false
			s.probeCancel = nil
			s.activityMu.Unlock()
		}()
		s.probeAccount(ctx, account)
	}()
	return true
}

func startResilientWorker(ctx context.Context, name string, run func()) {
	go func() {
		for ctx.Err() == nil {
			panicked := false
			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						panicked = true
						slog.Error("background worker panic recovered; restarting", "worker", name, "panic", recovered)
					}
				}()
				run()
			}()
			if !panicked || ctx.Err() != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()
}

func (s *Service) Register(r *gin.Engine) {
	r.GET("/ready", func(c *gin.Context) {
		if err := s.db.PingContext(c.Request.Context()); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "degraded", "database": "unavailable", "proxy_config": "in-memory"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready", "database": "ok"})
	})
	admin := r.Group("/admin/api", s.adminAuth())
	admin.GET("/overview", s.overview)
	admin.GET("/accounts", s.listAccounts)
	admin.POST("/accounts", s.createAccount)
	admin.PUT("/accounts/:id", s.updateAccount)
	admin.DELETE("/accounts/:id", s.deleteAccount)
	admin.POST("/accounts/:id/probe", s.probeOne)
	admin.GET("/model-pools", s.listPools)
	admin.PUT("/model-pools", s.updatePool)
	admin.GET("/requests", s.listRequests)

	r.GET("/v1/models", s.gatewayAuth(), s.models)
	r.POST("/v1/chat/completions", s.gatewayAuth(), s.chatCompletions)
}

func (s *Service) adminAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		provided := c.GetHeader("X-Admin-Password")
		if provided == "" {
			provided = strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
		}
		if !constantEqual(provided, s.adminPassword) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		c.Next()
	}
}

func (s *Service) gatewayAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !constantEqual(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "), s.clientKey) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid API key"})
			return
		}
		c.Next()
	}
}

func (s *Service) reload() error {
	rows, err := s.db.Query(`SELECT id,name,base_url,api_key_enc,public_model,upstream_model,enabled,priority,ttft_ewma_ms,total_ewma_ms,samples,successes,failures,consecutive_failures,cooldown_until,last_error FROM accounts`)
	if err != nil {
		return err
	}
	accounts := make(map[string]Account)
	for rows.Next() {
		var a Account
		var enc, cooldown string
		var enabled int
		if err := rows.Scan(&a.ID, &a.Name, &a.BaseURL, &enc, &a.PublicModel, &a.UpstreamModel, &enabled, &a.Priority, &a.TTFTEWMA, &a.TotalEWMA, &a.Samples, &a.Successes, &a.Failures, &a.ConsecutiveFail, &cooldown, &a.LastError); err != nil {
			rows.Close()
			return err
		}
		secret, err := s.decrypt(enc)
		if err != nil {
			rows.Close()
			return fmt.Errorf("decrypt account %s: %w", a.Name, err)
		}
		a.apiKey, a.APIKeySet, a.Enabled = secret, secret != "", enabled != 0
		if cooldown != "" {
			a.CooldownUntil, _ = time.Parse(time.RFC3339Nano, cooldown)
		}
		accounts[a.ID] = a
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	poolRows, err := s.db.Query(`SELECT public_model,strategy,timeout_ms,request_timeout_ms,max_attempts,failure_threshold,cooldown_seconds,probe_interval_seconds FROM pools`)
	if err != nil {
		return err
	}
	pools := make(map[string]Pool)
	for poolRows.Next() {
		var p Pool
		if err := poolRows.Scan(&p.PublicModel, &p.Strategy, &p.TimeoutMS, &p.RequestTimeoutMS, &p.MaxAttempts, &p.FailureThreshold, &p.CooldownSeconds, &p.ProbeIntervalSecs); err != nil {
			poolRows.Close()
			return err
		}
		pools[p.PublicModel] = p
	}
	if err := poolRows.Err(); err != nil {
		poolRows.Close()
		return err
	}
	poolRows.Close()
	s.mu.Lock()
	s.accounts, s.pools = accounts, pools
	s.mu.Unlock()
	return nil
}

func (s *Service) models(c *gin.Context) {
	s.mu.RLock()
	models := make([]string, 0, len(s.pools))
	for model := range s.pools {
		models = append(models, model)
	}
	s.mu.RUnlock()
	sort.Strings(models)
	data := make([]gin.H, 0, len(models))
	for _, model := range models {
		data = append(data, gin.H{"id": model, "object": "model", "owned_by": "keylane"})
	}
	c.JSON(http.StatusOK, gin.H{"object": "list", "data": data})
}

func (s *Service) overview(c *gin.Context) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	enabled, healthy, cooling := 0, 0, 0
	for _, a := range s.accounts {
		if !a.Enabled {
			continue
		}
		enabled++
		if a.CooldownUntil.After(time.Now()) {
			cooling++
		} else {
			healthy++
		}
	}
	c.JSON(http.StatusOK, gin.H{"accounts": len(s.accounts), "enabled": enabled, "healthy": healthy, "cooling": cooling, "model_pools": len(s.pools)})
}

func (s *Service) listAccounts(c *gin.Context) {
	s.mu.RLock()
	list := make([]Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		a.apiKey = ""
		list = append(list, a)
	}
	s.mu.RUnlock()
	sort.Slice(list, func(i, j int) bool {
		if list[i].PublicModel == list[j].PublicModel {
			return list[i].Priority < list[j].Priority
		}
		return list[i].PublicModel < list[j].PublicModel
	})
	c.JSON(http.StatusOK, gin.H{"data": list})
}

func (s *Service) createAccount(c *gin.Context) {
	var in AccountInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if err := validateAccount(in, true); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	id := randomID()
	enc, err := s.encrypt(in.APIKey)
	if err != nil {
		c.JSON(500, gin.H{"error": "credential encryption failed"})
		return
	}
	enabled := 1
	if in.Enabled != nil && !(*in.Enabled) {
		enabled = 0
	}
	if in.Priority == 0 {
		in.Priority = 100
	}
	_, err = s.db.Exec(`INSERT INTO accounts(id,name,base_url,api_key_enc,public_model,upstream_model,enabled,priority,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, id, strings.TrimSpace(in.Name), normalizeBaseURL(in.BaseURL), enc, strings.TrimSpace(in.PublicModel), strings.TrimSpace(in.UpstreamModel), enabled, in.Priority, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		c.JSON(500, gin.H{"error": "could not save account"})
		return
	}
	_, _ = s.db.Exec(`INSERT OR IGNORE INTO pools(public_model,strategy,timeout_ms,request_timeout_ms,max_attempts,failure_threshold,cooldown_seconds,probe_interval_seconds) VALUES(?,?,?,?,?,?,?,?)`, in.PublicModel, "fastest", int(s.defaultTimeout.Milliseconds()), int(s.defaultRequestTimeout.Milliseconds()), s.defaultAttempts, s.defaultFailure, s.defaultCooldown, int(s.probeInterval.Seconds()))
	if err := s.reload(); err != nil {
		c.JSON(500, gin.H{"error": "saved but runtime reload failed"})
		return
	}
	c.JSON(201, gin.H{"id": id})
}

func (s *Service) updateAccount(c *gin.Context) {
	id := c.Param("id")
	old, ok := s.account(id)
	if !ok {
		c.JSON(404, gin.H{"error": "account not found"})
		return
	}
	var in AccountInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if in.APIKey == "" {
		in.APIKey = old.apiKey
	}
	if in.Name == "" {
		in.Name = old.Name
	}
	if in.BaseURL == "" {
		in.BaseURL = old.BaseURL
	}
	if in.PublicModel == "" {
		in.PublicModel = old.PublicModel
	}
	if in.UpstreamModel == "" {
		in.UpstreamModel = old.UpstreamModel
	}
	if err := validateAccount(in, false); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	enc, err := s.encrypt(in.APIKey)
	if err != nil {
		c.JSON(500, gin.H{"error": "credential encryption failed"})
		return
	}
	enabled := old.Enabled
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	en := 0
	if enabled {
		en = 1
	}
	priority := in.Priority
	if priority == 0 {
		priority = old.Priority
	}
	_, err = s.db.Exec(`UPDATE accounts SET name=?,base_url=?,api_key_enc=?,public_model=?,upstream_model=?,enabled=?,priority=? WHERE id=?`, strings.TrimSpace(in.Name), normalizeBaseURL(in.BaseURL), enc, strings.TrimSpace(in.PublicModel), strings.TrimSpace(in.UpstreamModel), en, priority, id)
	if err != nil {
		c.JSON(500, gin.H{"error": "could not update account"})
		return
	}
	_, _ = s.db.Exec(`INSERT OR IGNORE INTO pools(public_model,strategy,timeout_ms,request_timeout_ms,max_attempts,failure_threshold,cooldown_seconds,probe_interval_seconds) VALUES(?,?,?,?,?,?,?,?)`, in.PublicModel, "fastest", int(s.defaultTimeout.Milliseconds()), int(s.defaultRequestTimeout.Milliseconds()), s.defaultAttempts, s.defaultFailure, s.defaultCooldown, int(s.probeInterval.Seconds()))
	if err := s.reload(); err != nil {
		c.JSON(500, gin.H{"error": "runtime reload failed"})
		return
	}
	c.Status(204)
}

func (s *Service) deleteAccount(c *gin.Context) {
	_, err := s.db.Exec(`DELETE FROM accounts WHERE id=?`, c.Param("id"))
	if err != nil {
		c.JSON(500, gin.H{"error": "could not delete account"})
		return
	}
	_ = s.reload()
	c.Status(204)
}

func (s *Service) listPools(c *gin.Context) {
	s.mu.RLock()
	list := make([]Pool, 0, len(s.pools))
	for _, p := range s.pools {
		list = append(list, p)
	}
	s.mu.RUnlock()
	sort.Slice(list, func(i, j int) bool { return list[i].PublicModel < list[j].PublicModel })
	c.JSON(200, gin.H{"data": list})
}

func (s *Service) updatePool(c *gin.Context) {
	var in PoolInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	model := in.PublicModel
	if model == "" {
		c.JSON(400, gin.H{"error": "invalid model"})
		return
	}
	old, ok := s.pool(model)
	if !ok {
		c.JSON(404, gin.H{"error": "model pool not found"})
		return
	}
	if in.Strategy == "" {
		in.Strategy = old.Strategy
	}
	if in.TimeoutMS == 0 {
		in.TimeoutMS = old.TimeoutMS
	}
	if in.RequestTimeoutMS == 0 {
		in.RequestTimeoutMS = old.RequestTimeoutMS
	}
	if in.MaxAttempts == 0 {
		in.MaxAttempts = old.MaxAttempts
	}
	if in.FailureThreshold == 0 {
		in.FailureThreshold = old.FailureThreshold
	}
	if in.CooldownSeconds == 0 {
		in.CooldownSeconds = old.CooldownSeconds
	}
	if in.Strategy != "fastest" && in.Strategy != "ordered" {
		c.JSON(400, gin.H{"error": "strategy must be fastest or ordered"})
		return
	}
	if in.TimeoutMS < 1000 || in.TimeoutMS > 3600000 || in.RequestTimeoutMS < 1000 || in.RequestTimeoutMS > 7200000 || in.MaxAttempts < 1 || in.MaxAttempts > 20 || in.FailureThreshold < 1 || in.FailureThreshold > 20 || in.CooldownSeconds < 1 || in.ProbeIntervalSecs < 0 || in.ProbeIntervalSecs > 86400 || (in.ProbeIntervalSecs > 0 && in.ProbeIntervalSecs < 30) {
		c.JSON(400, gin.H{"error": "pool parameters are outside supported ranges"})
		return
	}
	_, err := s.db.Exec(`UPDATE pools SET strategy=?,timeout_ms=?,request_timeout_ms=?,max_attempts=?,failure_threshold=?,cooldown_seconds=?,probe_interval_seconds=? WHERE public_model=?`, in.Strategy, in.TimeoutMS, in.RequestTimeoutMS, in.MaxAttempts, in.FailureThreshold, in.CooldownSeconds, in.ProbeIntervalSecs, model)
	if err != nil {
		c.JSON(500, gin.H{"error": "could not save model pool"})
		return
	}
	if err := s.reload(); err != nil {
		c.JSON(500, gin.H{"error": "runtime reload failed"})
		return
	}
	c.Status(204)
}

func (s *Service) listRequests(c *gin.Context) {
	rows, err := s.db.Query(`SELECT at,public_model,account_name,status,ttft_ms,total_ms,attempts,error_type,error_message FROM request_events ORDER BY id DESC LIMIT 100`)
	if err != nil {
		c.JSON(500, gin.H{"error": "request history unavailable"})
		return
	}
	defer rows.Close()
	type item struct {
		At        string  `json:"at"`
		Model     string  `json:"model"`
		Account   string  `json:"account"`
		Status    int     `json:"status"`
		TTFT      float64 `json:"ttft_ms"`
		Total     float64 `json:"total_ms"`
		Attempts  int     `json:"attempts"`
		ErrorType string  `json:"error_type"`
		Error     string  `json:"error"`
	}
	items := []item{}
	for rows.Next() {
		var x item
		if rows.Scan(&x.At, &x.Model, &x.Account, &x.Status, &x.TTFT, &x.Total, &x.Attempts, &x.ErrorType, &x.Error) == nil {
			items = append(items, x)
		}
	}
	c.JSON(200, gin.H{"data": items})
}

func (s *Service) probeOne(c *gin.Context) {
	a, ok := s.account(c.Param("id"))
	if !ok {
		c.JSON(404, gin.H{"error": "account not found"})
		return
	}
	if !s.startProbe(context.Background(), a) {
		c.JSON(http.StatusConflict, gin.H{"error": "probe waits until the gateway is idle"})
		return
	}
	c.JSON(202, gin.H{"status": "queued"})
}

func (s *Service) account(id string) (Account, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.accounts[id]
	return a, ok
}
func (s *Service) pool(model string) (Pool, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.pools[model]
	return p, ok
}

func (s *Service) encrypt(plain string) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := s.aead.Seal(nonce, nonce, []byte(plain), nil)
	return base64.RawStdEncoding.EncodeToString(sealed), nil
}
func (s *Service) decrypt(value string) (string, error) {
	raw, err := base64.RawStdEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	n := s.aead.NonceSize()
	if len(raw) < n {
		return "", errors.New("invalid encrypted credential")
	}
	plain, err := s.aead.Open(nil, raw[:n], raw[n:], nil)
	return string(plain), err
}

func (s *Service) metricWriter(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-s.metricQueue:
			_, _ = s.db.Exec(`UPDATE accounts SET ttft_ewma_ms=?,total_ewma_ms=?,samples=?,successes=?,failures=?,consecutive_failures=?,cooldown_until=?,last_error=? WHERE id=?`, ev.MetricTTFT, ev.MetricTotal, ev.MetricSamples, ev.MetricSuccess, ev.MetricFailures, ev.Consecutive, formatTime(ev.CooldownUntil), ev.ErrorMessage, ev.AccountID)
		}
	}
}

func (s *Service) requestWriter(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-s.requestQueue:
			_, _ = s.db.Exec(`INSERT INTO request_events(at,public_model,account_id,account_name,status,ttft_ms,total_ms,attempts,error_type,error_message) VALUES(?,?,?,?,?,?,?,?,?,?)`, ev.At.UTC().Format(time.RFC3339Nano), ev.PublicModel, ev.AccountID, ev.AccountName, ev.Status, ev.TTFTMS, ev.TotalMS, ev.Attempts, ev.ErrorType, truncate(ev.ErrorMessage, 300))
		}
	}
}

func (s *Service) record(ev requestEvent) {
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	if ev.AccountID != "" {
		s.mu.Lock()
		a, exists := s.accounts[ev.AccountID]
		if exists {
			if ev.Successful {
				a.Samples++
				a.Successes++
				a.ConsecutiveFail = 0
				a.CooldownUntil = time.Time{}
				a.LastError = ""
				if ev.TTFTMS > 0 {
					a.TTFTEWMA = ewma(a.TTFTEWMA, ev.TTFTMS, a.Samples)
				}
				if ev.TotalMS > 0 {
					a.TotalEWMA = ewma(a.TotalEWMA, ev.TotalMS, a.Samples)
				}
			} else {
				a.Failures++
				a.ConsecutiveFail++
				a.LastError = ev.ErrorMessage
				pool := s.pools[ev.PublicModel]
				threshold := pool.FailureThreshold
				if threshold < 1 {
					threshold = s.defaultFailure
				}
				if a.ConsecutiveFail >= threshold {
					cooldown := pool.CooldownSeconds
					if cooldown < 1 {
						cooldown = s.defaultCooldown
					}
					a.CooldownUntil = time.Now().Add(time.Duration(cooldown) * time.Second)
				}
			}
			ev.Consecutive = a.ConsecutiveFail
			ev.CooldownUntil = a.CooldownUntil
			ev.MetricTTFT = a.TTFTEWMA
			ev.MetricTotal = a.TotalEWMA
			ev.MetricSamples = a.Samples
			ev.MetricSuccess = a.Successes
			ev.MetricFailures = a.Failures
			s.accounts[ev.AccountID] = a
		}
		s.mu.Unlock()
		if exists {
			select {
			case s.metricQueue <- ev:
			default:
			}
		}
	}
	if !ev.Probe {
		select {
		case s.requestQueue <- ev:
		default:
		}
	}
}

func ewma(old, value float64, samples int64) float64 {
	if samples <= 1 || old == 0 {
		return value
	}
	return .25*value + .75*old
}
func constantEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", b)
}
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
func normalizeBaseURL(v string) string { return strings.TrimRight(strings.TrimSpace(v), "/") }
func envInt(k string, def, min, max int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n < min || n > max {
		return def
	}
	return n
}
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
func recoverWorker(name string) {
	if v := recover(); v != nil {
		slog.Error("worker panic recovered", "worker", name, "panic", v)
	}
}

func validateAccount(in AccountInput, requireKey bool) error {
	if strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.PublicModel) == "" || strings.TrimSpace(in.UpstreamModel) == "" {
		return errors.New("name, public_model, and upstream_model are required")
	}
	u, err := url.ParseRequestURI(normalizeBaseURL(in.BaseURL))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return errors.New("base_url must be an absolute http(s) URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("base_url scheme must be http or https")
	}
	if requireKey && strings.TrimSpace(in.APIKey) == "" {
		return errors.New("api_key is required")
	}
	return nil
}

func jsonBody(v any) ([]byte, error) { return json.Marshal(v) }
func boundedRead(r io.Reader, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, limit+1))
}
func bodyReader(b []byte) io.Reader { return bytes.NewReader(b) }
func safeError(err error) string {
	if err == nil {
		return ""
	}
	return classifyError(err)
}
func finite(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}
