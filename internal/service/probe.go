package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

func (s *Service) probeLoop(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.mu.RLock()
			accounts := make([]Account, 0, len(s.accounts))
			for _, account := range s.accounts {
				if account.Enabled {
					accounts = append(accounts, account)
				}
			}
			pools := make(map[string]Pool, len(s.pools))
			for model, pool := range s.pools {
				pools[model] = pool
			}
			s.mu.RUnlock()
			for _, account := range accounts {
				pool := pools[account.PublicModel]
				if pool.ProbeIntervalSecs == 0 {
					continue
				}
				interval := time.Duration(pool.ProbeIntervalSecs) * time.Second
				if interval <= 0 {
					interval = s.probeInterval
				}
				s.mu.RLock()
				last := s.probeLast[account.ID]
				s.mu.RUnlock()
				if (last.IsZero() || now.Sub(last) >= interval) && s.startProbe(ctx, account) {
					s.mu.Lock()
					s.probeLast[account.ID] = now
					s.mu.Unlock()
					break
				}
			}
		}
	}
}

// probeOutcome is the result of one live probe. It is returned to the admin
// UI for the on-demand "test speed" action and also recorded as a metric.
type probeOutcome struct {
	OK        bool    `json:"ok"`
	Status    int     `json:"status"`
	TTFTMS    float64 `json:"ttft_ms"`
	TotalMS   float64 `json:"total_ms"`
	ErrorType string  `json:"error_type"`
	Error     string  `json:"error"`
}

// runProbe performs a single small streaming request against the upstream and
// returns its timing/health. It never changes routing state; metrics are
// updated by the caller through s.record.
func (s *Service) runProbe(parent context.Context, account Account) probeOutcome {
	timeout := s.probeTimeout
	if pool, ok := s.pool(account.PublicModel); ok && pool.TimeoutMS > 0 && time.Duration(pool.TimeoutMS)*time.Millisecond < timeout {
		timeout = time.Duration(pool.TimeoutMS) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	started := time.Now()
	payload, _ := json.Marshal(map[string]any{
		"model":      account.UpstreamModel,
		"messages":   []map[string]string{{"role": "user", "content": "Reply with OK."}},
		"max_tokens": 8,
		"stream":     true,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatURL(account.BaseURL), bytes.NewReader(payload))
	if err != nil {
		return probeOutcome{ErrorType: "invalid_upstream_url", Error: safeError(err)}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+account.apiKey)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		if parent.Err() != nil {
			return probeOutcome{}
		}
		return probeOutcome{ErrorType: classifyError(err), Error: safeError(err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = boundedRead(resp.Body, 512)
		return probeOutcome{Status: resp.StatusCode, ErrorType: classifyStatus(resp.StatusCode), Error: "probe returned " + resp.Status}
	}
	buf := make([]byte, 4096)
	first := float64(0)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 && first == 0 {
			first = float64(time.Since(started).Milliseconds())
		}
		if readErr == io.EOF {
			return probeOutcome{OK: true, Status: resp.StatusCode, TTFTMS: first, TotalMS: float64(time.Since(started).Milliseconds())}
		}
		if readErr != nil {
			if parent.Err() != nil {
				return probeOutcome{}
			}
			return probeOutcome{Status: resp.StatusCode, TTFTMS: first, ErrorType: classifyError(readErr), Error: safeError(readErr)}
		}
	}
}

func (s *Service) recordProbe(account Account, out probeOutcome) {
	ev := requestEvent{
		At: time.Now(), PublicModel: account.PublicModel, AccountID: account.ID,
		AccountName: account.Name, Probe: true, Status: out.Status,
		TTFTMS: out.TTFTMS, TotalMS: out.TotalMS, Successful: out.OK,
		ErrorType: out.ErrorType, ErrorMessage: out.Error,
	}
	s.record(ev)
}

// probeAccount is the background probe path. It ignores the returned value
// but still feeds metrics so routing keeps its latency estimates.
func (s *Service) probeAccount(parent context.Context, account Account) {
	if _, ok := s.pool(account.PublicModel); !ok || !account.Enabled {
		return
	}
	out := s.runProbe(parent, account)
	if parent.Err() != nil && !out.OK && out.ErrorType == "" {
		return
	}
	s.recordProbe(account, out)
}
