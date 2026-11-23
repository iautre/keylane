package main

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/gin-gonic/gin"
	"github.com/iautre/gowk"
	"github.com/iautre/keylane/internal/service"
	"github.com/iautre/keylane/internal/webui"
	_ "modernc.org/sqlite"
)

func main() {
	if os.Getenv("DATABASE_ENABLED") != "false" {
		slog.Error("KeyLane requires DATABASE_ENABLED=false before process startup; the app uses SQLite. Export it in the environment first.")
		os.Exit(1)
	}

	dbPath := env("DATABASE_PATH", "./data/keylane.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		slog.Error("create database directory", "error", err)
		os.Exit(1)
	}
	adminPassword := os.Getenv("ADMIN_PASSWORD")
	clientKey := os.Getenv("GATEWAY_API_KEY")
	encryptionSecret := os.Getenv("ENCRYPTION_SECRET")
	if adminPassword == "" || clientKey == "" || len(encryptionSecret) < 32 {
		slog.Error("ADMIN_PASSWORD, GATEWAY_API_KEY, and ENCRYPTION_SECRET (at least 32 chars) are required")
		os.Exit(1)
	}

	svc, err := service.New(dbPath, adminPassword, clientKey, encryptionSecret)
	if err != nil {
		slog.Error("initialize service", "error", err)
		os.Exit(1)
	}
	defer svc.Close()

	engine := gin.New()
	engine.Use(recoverRequest())
	engine.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	svc.Register(engine)
	registerWeb(engine)

	// Background workers have an independent shutdown context; gowk.Run owns HTTP lifecycle.
	workerCtx, stopWorkers := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopWorkers()
	svc.StartWorkers(workerCtx)

	gowk.Run(&gowk.ServerConfig{HttpEngine: engine})
}

func recoverRequest() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("request panic recovered", "path", c.Request.URL.Path, "panic", recovered)
				if !c.Writer.Written() {
					c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"type": "gateway_error", "message": "internal proxy error"}})
				} else {
					c.Abort()
				}
			}
		}()
		c.Next()
	}
}

func registerWeb(r *gin.Engine) {
	dist, err := fs.Sub(webui.Files, "dist")
	if err != nil {
		slog.Error("missing embedded web build", "error", err)
		os.Exit(1)
	}
	assets, err := fs.Sub(dist, "assets")
	if err != nil {
		slog.Error("missing embedded web assets", "error", err)
		os.Exit(1)
	}
	r.StaticFS("/assets", http.FS(assets))
	r.NoRoute(func(c *gin.Context) {
		if c.Request.Method == http.MethodGet && (c.Request.URL.Path == "/" || strings.Contains(c.GetHeader("Accept"), "text/html")) {
			index, err := fs.ReadFile(dist, "index.html")
			if err != nil {
				c.Status(http.StatusServiceUnavailable)
				return
			}
			c.Data(http.StatusOK, "text/html; charset=utf-8", index)
			return
		}
		c.Status(http.StatusNotFound)
	})
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
