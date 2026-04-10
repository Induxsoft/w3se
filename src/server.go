package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Server is the main HTTP server for w3se.
type Server struct {
	cfg        Config
	registry   *Registry
	dispatcher *WebhookDispatcher
	logger     *Logger
	mux        *http.ServeMux
	channels   map[string]ChannelConfig // keyed by name
	limiters   map[string]*RateLimiter  // keyed by channel name
	startTime  time.Time
}

func NewServer(cfg Config, logger *Logger) *Server {
	s := &Server{
		cfg:        cfg,
		registry:   NewRegistry(cfg.Server.MaxConnections),
		dispatcher: NewWebhookDispatcher(logger),
		logger:     logger,
		mux:        http.NewServeMux(),
		channels:   make(map[string]ChannelConfig),
		limiters:   make(map[string]*RateLimiter),
		startTime:  time.Now(),
	}

	// Index channels and create rate limiters
	for _, ch := range cfg.Channels {
		s.channels[ch.Name] = ch
		if ch.Auth.RateLimit.RequestsPerMinute > 0 {
			s.limiters[ch.Name] = NewRateLimiter(ch.Auth.RateLimit.RequestsPerMinute)
		}
	}

	// Register channel routes
	for _, ch := range cfg.Channels {
		ch := ch // capture
		s.mux.HandleFunc(ch.Paths.SSE, func(w http.ResponseWriter, r *http.Request) {
			s.handleSSE(w, r, ch)
		})
		s.mux.HandleFunc(ch.Paths.WS, func(w http.ResponseWriter, r *http.Request) {
			s.handleWS(w, r, ch)
		})
	}

	// Register global routes
	s.mux.HandleFunc("/connections/", s.handleConnections)
	s.mux.HandleFunc("/health", s.handleHealth)

	// Start rate limiter cleanup
	go s.cleanupLoop()

	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// handleSSE handles incoming SSE connections.
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request, ch ChannelConfig) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Auth check
	if !s.authenticateChannel(w, r, ch) {
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	connID := generateID()
	remoteIP := s.resolveIP(r)

	conn := &Connection{
		ID:          connID,
		Channel:     ch.Name,
		Type:        ConnTypeSSE,
		RemoteIP:    remoteIP,
		ConnectedAt: time.Now(),
		LastActivity: time.Now(),
		Queue:       make(chan []byte, 256),
		Done:        make(chan struct{}),
		writer:      w,
		flusher:     flusher,
	}

	// Send webhook connection.opened (synchronous — backend decides accept/reject)
	openedPayload := WebhookOpened{
		Event:        "connection.opened",
		Channel:      ch.Name,
		ConnectionID: connID,
		Type:         ConnTypeSSE,
		RemoteIP:     remoteIP,
		Path:         r.URL.Path,
		Headers:      extractHeaders(r),
		QueryParams:  extractQueryParams(r),
		Timestamp:    time.Now().UTC().Format(time.RFC3339Nano),
	}

	status, err := s.dispatcher.SendSync(ch.Webhooks, openedPayload)
	if err != nil {
		s.logger.Error("webhook connection.opened failed",
			"channel", ch.Name, "error", err.Error())
		http.Error(w, "backend unavailable", http.StatusBadGateway)
		return
	}
	if status < 200 || status >= 300 {
		s.logger.Info("connection rejected by backend",
			"channel", ch.Name, "connection_id", connID, "status", status)
		http.Error(w, "connection rejected", http.StatusForbidden)
		return
	}

	// Register connection
	if err := s.registry.Add(conn); err != nil {
		s.logger.Error("cannot register connection",
			"channel", ch.Name, "error", err.Error())
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return
	}

	s.logger.Info("connection opened",
		"channel", ch.Name, "connection_id", connID, "type", ConnTypeSSE, "remote_ip", remoteIP)

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Connection-ID", connID)
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// If POST with body, send connection.message webhook
	if r.Method == http.MethodPost && r.Body != nil {
		body, err := io.ReadAll(io.LimitReader(r.Body, int64(s.cfg.Server.MaxMessageSizeBytes)))
		if err == nil && len(body) > 0 {
			msgPayload := buildMessagePayload(ch.Name, connID, body)
			s.dispatcher.SendAsync(ch.Webhooks, msgPayload)
		}
	}

	// Start write loop in goroutine
	go conn.WriteLoop()

	// Heartbeat and idle timeout
	heartbeat := time.NewTicker(time.Duration(ch.Connections.HeartbeatIntervalMs) * time.Millisecond)
	defer heartbeat.Stop()

	idleTimeout := time.NewTimer(time.Duration(ch.Connections.IdleTimeoutMs) * time.Millisecond)
	defer idleTimeout.Stop()

	// Wait for client disconnect, idle timeout, or server close
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			s.closeConnection(conn, "client_disconnect", ch)
			return
		case <-conn.Done:
			return
		case <-heartbeat.C:
			if err := conn.SendHeartbeat(); err != nil {
				s.closeConnection(conn, "error", ch)
				return
			}
		case <-idleTimeout.C:
			s.closeConnection(conn, "idle_timeout", ch)
			return
		}
	}
}

// handleWS handles incoming WebSocket connections.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request, ch ChannelConfig) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Auth check
	if !s.authenticateChannel(w, r, ch) {
		return
	}

	connID := generateID()
	remoteIP := s.resolveIP(r)

	// Send webhook connection.opened before upgrading
	openedPayload := WebhookOpened{
		Event:        "connection.opened",
		Channel:      ch.Name,
		ConnectionID: connID,
		Type:         ConnTypeWS,
		RemoteIP:     remoteIP,
		Path:         r.URL.Path,
		Headers:      extractHeaders(r),
		QueryParams:  extractQueryParams(r),
		Timestamp:    time.Now().UTC().Format(time.RFC3339Nano),
	}

	status, err := s.dispatcher.SendSync(ch.Webhooks, openedPayload)
	if err != nil {
		s.logger.Error("webhook connection.opened failed",
			"channel", ch.Name, "error", err.Error())
		http.Error(w, "backend unavailable", http.StatusBadGateway)
		return
	}
	if status < 200 || status >= 300 {
		s.logger.Info("connection rejected by backend",
			"channel", ch.Name, "connection_id", connID, "status", status)
		http.Error(w, "connection rejected", http.StatusForbidden)
		return
	}

	// Upgrade to WebSocket
	wsConn, err := UpgradeWebSocket(w, r, http.Header{
		"X-Connection-ID": []string{connID},
	})
	if err != nil {
		s.logger.Error("websocket upgrade failed",
			"channel", ch.Name, "error", err.Error())
		return
	}

	conn := &Connection{
		ID:          connID,
		Channel:     ch.Name,
		Type:        ConnTypeWS,
		RemoteIP:    remoteIP,
		ConnectedAt: time.Now(),
		LastActivity: time.Now(),
		Queue:       make(chan []byte, 256),
		Done:        make(chan struct{}),
		wsConn:      wsConn,
	}

	if err := s.registry.Add(conn); err != nil {
		s.logger.Error("cannot register connection",
			"channel", ch.Name, "error", err.Error())
		wsConn.Close()
		return
	}

	s.logger.Info("connection opened",
		"channel", ch.Name, "connection_id", connID, "type", ConnTypeWS, "remote_ip", remoteIP)

	// Start write loop
	go conn.WriteLoop()

	// Start idle timeout watcher
	go s.watchIdleTimeout(conn, ch)

	// Read loop — reads messages from client and sends webhooks
	s.wsReadLoop(conn, ch)
}

func (s *Server) wsReadLoop(conn *Connection, ch ChannelConfig) {
	defer func() {
		s.closeConnection(conn, "client_disconnect", ch)
		conn.wsConn.Close()
	}()

	for {
		_, msg, err := conn.wsConn.ReadMessage()
		if err != nil {
			s.logger.Debug("websocket read error",
				"connection_id", conn.ID, "error", err.Error())
			return
		}

		conn.LastActivity = time.Now()

		// Send connection.message webhook
		msgPayload := buildMessagePayload(ch.Name, conn.ID, msg)
		s.dispatcher.SendAsync(ch.Webhooks, msgPayload)
	}
}

func (s *Server) watchIdleTimeout(conn *Connection, ch ChannelConfig) {
	timeout := time.Duration(ch.Connections.IdleTimeoutMs) * time.Millisecond
	ticker := time.NewTicker(timeout / 2)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if time.Since(conn.LastActivity) >= timeout {
				s.closeConnection(conn, "idle_timeout", ch)
				if conn.wsConn != nil {
					conn.wsConn.Close()
				}
				return
			}
		case <-conn.Done:
			return
		}
	}
}

// handleConnections routes /connections/{id}/messages and /connections/{id}
func (s *Server) handleConnections(w http.ResponseWriter, r *http.Request) {
	// Parse path: /connections/{id}/messages or /connections/{id}
	path := strings.TrimPrefix(r.URL.Path, "/connections/")
	parts := strings.SplitN(path, "/", 2)

	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "connection id required", http.StatusBadRequest)
		return
	}

	connID := parts[0]

	if len(parts) == 2 && parts[1] == "messages" {
		s.handleSendMessage(w, r, connID)
	} else if len(parts) == 1 {
		if r.Method == http.MethodDelete {
			s.handleDeleteConnection(w, r, connID)
		} else {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	} else {
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request, connID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	conn, ok := s.registry.Get(connID)
	if !ok {
		http.Error(w, "connection not found", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, int64(s.cfg.Server.MaxMessageSizeBytes)+1))
	if err != nil {
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}

	if len(body) > s.cfg.Server.MaxMessageSizeBytes {
		http.Error(w, "message too large", http.StatusRequestEntityTooLarge)
		return
	}

	if !conn.Send(body) {
		http.Error(w, "connection closed or queue full", http.StatusGone)
		return
	}

	s.logger.Debug("message enqueued",
		"connection_id", connID, "size", len(body))

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteConnection(w http.ResponseWriter, r *http.Request, connID string) {
	conn, ok := s.registry.Get(connID)
	if !ok {
		http.Error(w, "connection not found", http.StatusNotFound)
		return
	}

	ch, chOk := s.channels[conn.Channel]
	if chOk {
		s.closeConnection(conn, "server_close", ch)
	} else {
		conn.Close()
		s.registry.Remove(connID)
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	total, sse, ws := s.registry.Count()

	resp := map[string]any{
		"status": "ok",
		"connections": map[string]any{
			"active": total,
			"sse":    sse,
			"ws":     ws,
		},
		"uptime_seconds": int(time.Since(s.startTime).Seconds()),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// closeConnection removes a connection from the registry and sends the closed webhook.
func (s *Server) closeConnection(conn *Connection, reason string, ch ChannelConfig) {
	// Ensure we only close once
	alreadyClosed := false
	select {
	case <-conn.Done:
		alreadyClosed = true
	default:
	}

	conn.Close()
	s.registry.Remove(conn.ID)

	if alreadyClosed {
		return
	}

	s.logger.Info("connection closed",
		"channel", conn.Channel, "connection_id", conn.ID,
		"reason", reason, "type", conn.Type)

	closedPayload := WebhookClosed{
		Event:          "connection.closed",
		Channel:        conn.Channel,
		ConnectionID:   conn.ID,
		Type:           conn.Type,
		RemoteIP:       conn.RemoteIP,
		Reason:         reason,
		DurationSeconds: time.Since(conn.ConnectedAt).Seconds(),
		MessagesSent:   atomic.LoadInt64(&conn.MsgSent),
		Timestamp:      time.Now().UTC().Format(time.RFC3339Nano),
	}

	s.dispatcher.SendAsync(ch.Webhooks, closedPayload)
}

// authenticateChannel checks token and rate limit for a channel.
func (s *Server) authenticateChannel(w http.ResponseWriter, r *http.Request, ch ChannelConfig) bool {
	// Rate limit check
	if limiter, ok := s.limiters[ch.Name]; ok {
		ip := s.resolveIP(r)
		if !limiter.Allow(ip) {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return false
		}
	}

	// Token check
	if len(ch.Auth.Tokens) > 0 && ch.Auth.TokenHeader != "" {
		token := r.Header.Get(ch.Auth.TokenHeader)
		valid := false
		for _, t := range ch.Auth.Tokens {
			if token == t {
				valid = true
				break
			}
		}
		if !valid {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return false
		}
	}

	return true
}

// resolveIP extracts the client IP, respecting trusted proxies.
func (s *Server) resolveIP(r *http.Request) string {
	if len(s.cfg.Server.TrustedProxies) > 0 {
		// Check if request comes from a trusted proxy
		remoteHost := strings.Split(r.RemoteAddr, ":")[0]
		for _, proxy := range s.cfg.Server.TrustedProxies {
			if remoteHost == proxy {
				// Use X-Real-IP or X-Forwarded-For
				if ip := r.Header.Get("X-Real-IP"); ip != "" {
					return ip
				}
				if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
					parts := strings.Split(xff, ",")
					return strings.TrimSpace(parts[0])
				}
			}
		}
	}
	// Fallback to remote addr
	host := r.RemoteAddr
	if idx := strings.LastIndex(host, ":"); idx != -1 {
		host = host[:idx]
	}
	return host
}

// cleanupLoop periodically cleans expired rate limit buckets.
func (s *Server) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		for _, rl := range s.limiters {
			rl.Cleanup()
		}
	}
}

// Shutdown gracefully shuts down all connections.
func (s *Server) Shutdown() {
	conns := s.registry.All()
	s.logger.Info("shutting down", "active_connections", len(conns))

	for _, conn := range conns {
		ch, ok := s.channels[conn.Channel]
		if ok {
			s.closeConnection(conn, "server_shutdown", ch)
		} else {
			conn.Close()
			s.registry.Remove(conn.ID)
		}
		if conn.wsConn != nil {
			conn.wsConn.WriteClose(1001, "server shutdown")
			conn.wsConn.Close()
		}
	}
}

// Helper functions

func generateID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func extractHeaders(r *http.Request) map[string]string {
	headers := make(map[string]string)
	for k, v := range r.Header {
		if len(v) > 0 {
			headers[strings.ToLower(k)] = v[0]
		}
	}
	return headers
}

func extractQueryParams(r *http.Request) map[string]string {
	params := make(map[string]string)
	for k, v := range r.URL.Query() {
		if len(v) > 0 {
			params[k] = v[0]
		}
	}
	return params
}

func buildMessagePayload(channel, connID string, body []byte) WebhookMessage {
	// Try to parse as JSON; if it fails, wrap as string
	var msg json.RawMessage
	if json.Valid(body) {
		msg = body
	} else {
		quoted, _ := json.Marshal(string(body))
		msg = quoted
	}

	return WebhookMessage{
		Event:        "connection.message",
		Channel:      channel,
		ConnectionID: connID,
		Message:      msg,
		Timestamp:    time.Now().UTC().Format(time.RFC3339Nano),
	}
}
