package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// WebhookDispatcher sends webhook events to the backend.
type WebhookDispatcher struct {
	client *http.Client
	logger *Logger
}

func NewWebhookDispatcher(logger *Logger) *WebhookDispatcher {
	return &WebhookDispatcher{
		client: &http.Client{},
		logger: logger,
	}
}

// SendSync sends a webhook and waits for the response. Returns the HTTP status code.
func (d *WebhookDispatcher) SendSync(cfg ChannelWebhooks, payload any) (int, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("marshal webhook payload: %w", err)
	}

	client := &http.Client{
		Timeout: time.Duration(cfg.TimeoutMs) * time.Millisecond,
	}

	method := cfg.Method
	if method == "" {
		method = "POST"
	}

	req, err := http.NewRequest(method, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("create webhook request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("webhook request failed: %w", err)
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body) // drain

	return resp.StatusCode, nil
}

// SendAsync sends a webhook without waiting for a response.
func (d *WebhookDispatcher) SendAsync(cfg ChannelWebhooks, payload any) {
	go func() {
		status, err := d.SendSync(cfg, payload)
		if err != nil {
			d.logger.Error("webhook failed",
				"url", cfg.URL,
				"error", err.Error(),
			)
		} else if status >= 400 {
			d.logger.Error("webhook returned error",
				"url", cfg.URL,
				"status", status,
			)
		}
	}()
}

// RateLimiter implements a simple per-key token bucket rate limiter.
type RateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	rate     int
	interval time.Duration
}

type bucket struct {
	tokens    int
	lastReset time.Time
}

func NewRateLimiter(requestsPerMinute int) *RateLimiter {
	if requestsPerMinute <= 0 {
		return nil
	}
	return &RateLimiter{
		buckets:  make(map[string]*bucket),
		rate:     requestsPerMinute,
		interval: time.Minute,
	}
}

// Allow returns true if the key has not exceeded the rate limit.
func (rl *RateLimiter) Allow(key string) bool {
	if rl == nil {
		return true
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	b, exists := rl.buckets[key]
	if !exists || now.Sub(b.lastReset) >= rl.interval {
		rl.buckets[key] = &bucket{tokens: rl.rate - 1, lastReset: now}
		return true
	}

	if b.tokens > 0 {
		b.tokens--
		return true
	}

	return false
}

// Cleanup removes expired buckets. Call periodically.
func (rl *RateLimiter) Cleanup() {
	if rl == nil {
		return
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	for k, b := range rl.buckets {
		if now.Sub(b.lastReset) >= rl.interval*2 {
			delete(rl.buckets, k)
		}
	}
}
