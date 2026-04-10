package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

const (
	ConnTypeSSE = "sse"
	ConnTypeWS  = "ws"
)

// Connection represents an active SSE or WebSocket connection.
type Connection struct {
	ID           string
	Channel      string
	Type         string
	RemoteIP     string
	ConnectedAt  time.Time
	LastActivity time.Time
	MsgSent      int64
	Queue        chan []byte
	Done         chan struct{}
	closeOnce    sync.Once

	// SSE-specific
	writer  http.ResponseWriter
	flusher http.Flusher

	// WS-specific
	wsConn *WSConn
}

// Send enqueues a message for delivery.
func (c *Connection) Send(data []byte) bool {
	select {
	case c.Queue <- data:
		c.LastActivity = time.Now()
		return true
	case <-c.Done:
		return false
	default:
		return false
	}
}

// Close signals the connection to shut down.
func (c *Connection) Close() {
	c.closeOnce.Do(func() {
		close(c.Done)
	})
}

// WriteLoop drains the queue and writes to the client.
func (c *Connection) WriteLoop() {
	for {
		select {
		case msg := <-c.Queue:
			var err error
			if c.Type == ConnTypeSSE {
				err = c.writeSSE(msg)
			} else {
				err = c.writeWS(msg)
			}
			if err != nil {
				return
			}
			atomic.AddInt64(&c.MsgSent, 1)
		case <-c.Done:
			return
		}
	}
}

func (c *Connection) writeSSE(data []byte) error {
	_, err := c.writer.Write(data)
	if err != nil {
		return err
	}
	// Ensure trailing newline for flush
	if len(data) == 0 || data[len(data)-1] != '\n' {
		c.writer.Write([]byte("\n"))
	}
	if c.flusher != nil {
		c.flusher.Flush()
	}
	return nil
}

func (c *Connection) writeWS(data []byte) error {
	return c.wsConn.WriteMessage(data)
}

// SendHeartbeat sends an SSE comment to keep the connection alive.
func (c *Connection) SendHeartbeat() error {
	if c.Type != ConnTypeSSE {
		return nil
	}
	_, err := fmt.Fprintf(c.writer, ": heartbeat\n\n")
	if err != nil {
		return err
	}
	if c.flusher != nil {
		c.flusher.Flush()
	}
	return nil
}

// Registry manages all active connections.
type Registry struct {
	connections map[string]*Connection
	mu          sync.RWMutex
	maxConns    int
}

func NewRegistry(maxConns int) *Registry {
	return &Registry{
		connections: make(map[string]*Connection),
		maxConns:    maxConns,
	}
}

func (r *Registry) Add(conn *Connection) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.connections) >= r.maxConns {
		return fmt.Errorf("max connections reached (%d)", r.maxConns)
	}

	r.connections[conn.ID] = conn
	return nil
}

func (r *Registry) Get(id string) (*Connection, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	conn, ok := r.connections[id]
	return conn, ok
}

func (r *Registry) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.connections, id)
}

func (r *Registry) All() []*Connection {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*Connection, 0, len(r.connections))
	for _, c := range r.connections {
		result = append(result, c)
	}
	return result
}

func (r *Registry) Count() (total, sse, ws int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, c := range r.connections {
		total++
		if c.Type == ConnTypeSSE {
			sse++
		} else {
			ws++
		}
	}
	return
}

// Webhook payload types

type WebhookOpened struct {
	Event        string            `json:"event"`
	Channel      string            `json:"channel"`
	ConnectionID string            `json:"connection_id"`
	Type         string            `json:"type"`
	RemoteIP     string            `json:"remote_ip"`
	Path         string            `json:"path"`
	Headers      map[string]string `json:"headers"`
	QueryParams  map[string]string `json:"query_params"`
	Timestamp    string            `json:"timestamp"`
}

type WebhookMessage struct {
	Event        string          `json:"event"`
	Channel      string          `json:"channel"`
	ConnectionID string          `json:"connection_id"`
	Message      json.RawMessage `json:"message"`
	Timestamp    string          `json:"timestamp"`
}

type WebhookClosed struct {
	Event           string  `json:"event"`
	Channel         string  `json:"channel"`
	ConnectionID    string  `json:"connection_id"`
	Type            string  `json:"type"`
	RemoteIP        string  `json:"remote_ip"`
	Reason          string  `json:"reason"`
	DurationSeconds float64 `json:"duration_seconds"`
	MessagesSent    int64   `json:"messages_sent"`
	Timestamp       string  `json:"timestamp"`
}
