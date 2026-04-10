package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

const (
	wsGUID          = "258EAFA5-E914-47DA-95CA-5AB5DC799C07"
	wsOpText        = 1
	wsOpClose       = 8
	wsOpPing        = 9
	wsOpPong        = 10
	wsMaxFrameSize  = 1 << 20 // 1MB
)

// WSConn wraps a hijacked net.Conn for WebSocket communication.
type WSConn struct {
	conn   net.Conn
	reader *bufio.Reader
	mu     sync.Mutex
}

// UpgradeWebSocket performs the WebSocket handshake and returns a WSConn.
func UpgradeWebSocket(w http.ResponseWriter, r *http.Request, responseHeaders http.Header) (*WSConn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil, errors.New("missing Upgrade: websocket header")
	}
	if !strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
		return nil, errors.New("missing Connection: upgrade header")
	}

	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, errors.New("missing Sec-WebSocket-Key header")
	}

	// Compute accept key
	h := sha1.New()
	h.Write([]byte(key + wsGUID))
	acceptKey := base64.StdEncoding.EncodeToString(h.Sum(nil))

	// Hijack the connection
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("server does not support hijacking")
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		return nil, fmt.Errorf("hijack failed: %w", err)
	}

	// Write handshake response
	var resp strings.Builder
	resp.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	resp.WriteString("Upgrade: websocket\r\n")
	resp.WriteString("Connection: Upgrade\r\n")
	resp.WriteString("Sec-WebSocket-Accept: " + acceptKey + "\r\n")
	for k, vals := range responseHeaders {
		for _, v := range vals {
			resp.WriteString(k + ": " + v + "\r\n")
		}
	}
	resp.WriteString("\r\n")

	if _, err := bufrw.WriteString(resp.String()); err != nil {
		conn.Close()
		return nil, fmt.Errorf("write handshake: %w", err)
	}
	if err := bufrw.Flush(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("flush handshake: %w", err)
	}

	return &WSConn{
		conn:   conn,
		reader: bufrw.Reader,
	}, nil
}

// ReadMessage reads the next text or close frame from the client.
// Returns the opcode and payload.
func (ws *WSConn) ReadMessage() (int, []byte, error) {
	for {
		opcode, payload, err := ws.readFrame()
		if err != nil {
			return 0, nil, err
		}

		switch opcode {
		case wsOpText:
			return opcode, payload, nil
		case wsOpClose:
			return opcode, payload, io.EOF
		case wsOpPing:
			// Respond with pong
			ws.writeFrame(wsOpPong, payload)
			continue
		case wsOpPong:
			continue
		default:
			// Binary or other — treat as text for our purposes
			return opcode, payload, nil
		}
	}
}

func (ws *WSConn) readFrame() (int, []byte, error) {
	// Read first 2 bytes
	header := make([]byte, 2)
	if _, err := io.ReadFull(ws.reader, header); err != nil {
		return 0, nil, err
	}

	opcode := int(header[0] & 0x0F)
	masked := (header[1] & 0x80) != 0
	length := uint64(header[1] & 0x7F)

	// Extended payload length
	switch length {
	case 126:
		ext := make([]byte, 2)
		if _, err := io.ReadFull(ws.reader, ext); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext))
	case 127:
		ext := make([]byte, 8)
		if _, err := io.ReadFull(ws.reader, ext); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext)
	}

	if length > wsMaxFrameSize {
		return 0, nil, fmt.Errorf("frame too large: %d bytes", length)
	}

	// Read mask key if present
	var maskKey [4]byte
	if masked {
		if _, err := io.ReadFull(ws.reader, maskKey[:]); err != nil {
			return 0, nil, err
		}
	}

	// Read payload
	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(ws.reader, payload); err != nil {
			return 0, nil, err
		}
	}

	// Unmask
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}

	return opcode, payload, nil
}

// WriteMessage sends a text frame to the client (server frames are unmasked).
func (ws *WSConn) WriteMessage(data []byte) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.writeFrame(wsOpText, data)
}

// WriteClose sends a close frame.
func (ws *WSConn) WriteClose(code int, reason string) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	payload := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(payload, uint16(code))
	copy(payload[2:], reason)
	return ws.writeFrame(wsOpClose, payload)
}

func (ws *WSConn) writeFrame(opcode int, payload []byte) error {
	// FIN + opcode
	frame := []byte{byte(0x80 | opcode)}

	// Payload length (server frames: no mask)
	length := len(payload)
	switch {
	case length <= 125:
		frame = append(frame, byte(length))
	case length <= 65535:
		frame = append(frame, 126)
		ext := make([]byte, 2)
		binary.BigEndian.PutUint16(ext, uint16(length))
		frame = append(frame, ext...)
	default:
		frame = append(frame, 127)
		ext := make([]byte, 8)
		binary.BigEndian.PutUint64(ext, uint64(length))
		frame = append(frame, ext...)
	}

	frame = append(frame, payload...)

	_, err := ws.conn.Write(frame)
	return err
}

// Close closes the underlying network connection.
func (ws *WSConn) Close() error {
	return ws.conn.Close()
}
