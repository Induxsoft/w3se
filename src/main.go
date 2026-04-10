package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	// Load configuration
	cfg, err := LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuration error: %s\n", err)
		os.Exit(1)
	}

	// Initialize logger
	logger, err := NewLogger(cfg.Logging)
	if err != nil {
		fmt.Fprintf(os.Stderr, "logger error: %s\n", err)
		os.Exit(1)
	}

	logger.Info("starting w3se",
		"host", cfg.Server.Host,
		"port", cfg.Server.Port,
		"tls", cfg.Server.TLS.Enabled,
		"channels", len(cfg.Channels),
	)

	for _, ch := range cfg.Channels {
		logger.Info("channel registered",
			"name", ch.Name,
			"sse_path", ch.Paths.SSE,
			"ws_path", ch.Paths.WS,
			"webhook_url", ch.Webhooks.URL,
		)
	}

	if len(cfg.Channels) == 0 {
		logger.Info("no channels configured — only /health is available")
	}

	// Create server
	app := NewServer(cfg, logger)

	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)

	httpServer := &http.Server{
		Addr:         addr,
		Handler:      app,
		ReadTimeout:  time.Duration(cfg.Server.ReadTimeoutMs) * time.Millisecond,
		WriteTimeout: 0, // Disable write timeout for SSE/WS long-lived connections
		IdleTimeout:  120 * time.Second,
	}

	if cfg.Server.TLS.Enabled {
		httpServer.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
	}

	// Start server in goroutine
	go func() {
		var err error
		if cfg.Server.TLS.Enabled {
			logger.Info("listening with TLS", "addr", addr)
			err = httpServer.ListenAndServeTLS(cfg.Server.TLS.Cert, cfg.Server.TLS.Key)
		} else {
			logger.Info("listening", "addr", addr)
			err = httpServer.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "error", err.Error())
			os.Exit(1)
		}
	}()

	// Wait for shutdown signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit

	logger.Info("shutdown signal received", "signal", sig.String())

	// Graceful shutdown
	shutdownWait := time.Duration(cfg.Server.ShutdownWaitMs) * time.Millisecond

	// First, close all w3se connections and notify backends
	app.Shutdown()

	// Then shutdown the HTTP server
	ctx, cancel := context.WithTimeout(context.Background(), shutdownWait)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		logger.Error("http server shutdown error", "error", err.Error())
	}

	logger.Info("w3se stopped")
}
