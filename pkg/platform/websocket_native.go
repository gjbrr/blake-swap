//go:build !js

package platform

import (
	"bufio"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

// WebSocket is a Conn over a host TCP socket.
//
// The browser has a WebSocket object; a host has a socket and a specification. Only
// what a relay client needs is implemented: text frames, close, ping/pong, and enough
// fragmentation to not misread a relay that splits a frame. Extensions, compression and
// binary application data are not negotiated, and the handshake does not ask for them.
type WebSocket struct {
	conn net.Conn
	read *bufio.Reader

	// writeMu serialises frames: a WebSocket frame is not atomic on the wire, and two
	// goroutines interleaving halves of one would corrupt the stream.
	writeMu sync.Mutex

	closeOnce sync.Once
	closeErr  error
}

// websocketGUID is the fixed string RFC 6455 mixes into the accept hash.
const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// SecWebSocketAccept is the header a server must answer the handshake with.
const secWebSocketAccept = "Sec-WebSocket-Accept"

// DialWebSocket opens one connection to a relay.
//
// The handshake is completed before returning, for the same reason the browser's dial
// waits for open: a caller that holds a Conn must be able to send immediately, and an
// unreachable relay should fail here where the error can name it.
func DialWebSocket(ctx context.Context, rawURL string) (*WebSocket, error) {
	parsed, err := url.Parse(rawURL)

	if err != nil {
		return nil, fmt.Errorf("websocket: %s is not a URL: %w", rawURL, err)
	}

	secure := false

	switch parsed.Scheme {
	case "wss":
		secure = true
	case "ws":
	default:
		return nil, fmt.Errorf("websocket: %s is not a ws:// or wss:// URL", rawURL)
	}

	if parsed.Host == "" {
		return nil, fmt.Errorf("websocket: %s has no host", rawURL)
	}

	address := parsed.Host

	if parsed.Port() == "" {
		if secure {
			address = net.JoinHostPort(parsed.Hostname(), "443")
		} else {
			address = net.JoinHostPort(parsed.Hostname(), "80")
		}
	}

	dialer := &net.Dialer{}

	conn, err := dialer.DialContext(ctx, "tcp", address)

	if err != nil {
		return nil, fmt.Errorf("websocket: %s: %w", rawURL, err)
	}

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	if secure {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName: parsed.Hostname(),
			MinVersion: tls.VersionTLS12,
		})

		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()

			return nil, fmt.Errorf("websocket: %s: %w", rawURL, err)
		}

		conn = tlsConn
	}

	socket := &WebSocket{conn: conn, read: bufio.NewReader(conn)}

	if err := socket.handshake(parsed); err != nil {
		_ = conn.Close()

		return nil, fmt.Errorf("websocket: %s: %w", rawURL, err)
	}

	// The handshake is done; per-call deadlines take over from here.
	_ = conn.SetDeadline(time.Time{})

	return socket, nil
}

// handshake performs the HTTP upgrade.
func (w *WebSocket) handshake(parsed *url.URL) error {
	nonce := make([]byte, 16)

	if _, err := ReadRandom(nonce); err != nil {
		return err
	}

	key := base64.StdEncoding.EncodeToString(nonce)

	path := parsed.RequestURI()

	if path == "" {
		path = "/"
	}

	request := strings.Join([]string{
		"GET " + path + " HTTP/1.1",
		"Host: " + parsed.Host,
		"Upgrade: websocket",
		"Connection: Upgrade",
		"Sec-WebSocket-Key: " + key,
		"Sec-WebSocket-Version: 13",
		"",
		"",
	}, "\r\n")

	if _, err := io.WriteString(w.conn, request); err != nil {
		return err
	}

	// Status line and headers, read by hand: the relay's answer is a handful of lines
	// and net/http would take over the connection this type needs to keep.
	status, err := w.read.ReadString('\n')

	if err != nil {
		return err
	}

	if !strings.Contains(status, " 101 ") {
		return fmt.Errorf("relay refused the upgrade: %s", strings.TrimSpace(status))
	}

	expected := acceptKey(key)
	accepted := false

	for {
		line, err := w.read.ReadString('\n')

		if err != nil {
			return err
		}

		line = strings.TrimRight(line, "\r\n")

		if line == "" {
			break
		}

		name, value, found := strings.Cut(line, ":")

		if !found {
			continue
		}

		if strings.EqualFold(strings.TrimSpace(name), secWebSocketAccept) &&
			strings.TrimSpace(value) == expected {
			accepted = true
		}
	}

	if !accepted {
		return fmt.Errorf("relay did not prove it read the handshake")
	}

	return nil
}

// acceptKey is the server's proof that it read the nonce.
func acceptKey(nonce string) string {
	sum := sha1.Sum([]byte(nonce + websocketGUID))

	return base64.StdEncoding.EncodeToString(sum[:])
}

// Send writes one masked text frame.
func (w *WebSocket) Send(ctx context.Context, payload string) error {
	if err := applyDeadline(w.conn, ctx); err != nil {
		return err
	}

	w.writeMu.Lock()
	defer w.writeMu.Unlock()

	return writeFrame(w.conn, opText, []byte(payload))
}

// Receive reads the next text frame.
//
// Control frames are handled here rather than surfaced: a ping must be answered to keep
// the connection alive, and a pong is not application data. A close returns io.EOF,
// which is how the relay client learns the subscription is over.
func (w *WebSocket) Receive(ctx context.Context) (string, error) {
	var assembled []byte

	for {
		if err := applyDeadline(w.conn, ctx); err != nil {
			return "", err
		}

		opcode, fin, payload, err := w.readFrame()

		if err != nil {
			return "", err
		}

		switch opcode {
		case opText, opBinary:
			assembled = payload

			if !fin {
				continue
			}

			return string(assembled), nil

		case opContinuation:
			assembled = append(assembled, payload...)

			if !fin {
				continue
			}

			return string(assembled), nil

		case opPing:
			w.writeMu.Lock()
			err := writeFrame(w.conn, opPong, payload)
			w.writeMu.Unlock()

			if err != nil {
				return "", err
			}

			continue

		case opPong:
			continue

		case opClose:
			_ = w.Close()

			return "", io.EOF
		}
	}
}

// Close shuts the connection down.
func (w *WebSocket) Close() error {
	w.closeOnce.Do(func() {
		w.writeMu.Lock()
		// A close frame is polite, and a relay that never gets one holds the
		// subscription open until a timer expires.
		_ = writeFrame(w.conn, opClose, nil)
		w.writeMu.Unlock()

		w.closeErr = w.conn.Close()
	})

	return w.closeErr
}

// The frame opcodes this client knows.
const (
	opContinuation byte = 0x0
	opText         byte = 0x1
	opBinary       byte = 0x2
	opClose        byte = 0x8
	opPing         byte = 0x9
	opPong         byte = 0xA
)

// writeFrame writes one client frame. Client frames are always masked.
func writeFrame(out io.Writer, opcode byte, payload []byte) error {
	header := make([]byte, 0, 14)
	header = append(header, 0x80|opcode)

	length := len(payload)

	switch {
	case length < 126:
		header = append(header, byte(length))
	case length <= 0xFFFF:
		header = append(header, 126, byte(length>>8), byte(length))
	default:
		header = append(header, 127)

		var wide [8]byte

		binary.BigEndian.PutUint64(wide[:], uint64(length))
		header = append(header, wide[:]...)
	}

	mask := make([]byte, 4)

	if _, err := ReadRandom(mask); err != nil {
		return err
	}

	// The mask bit is set for every client frame, and it shares its byte with the
	// length's high bits, so it is set after the length has been written.
	header[1] |= 0x80
	header = append(header, mask...)

	masked := make([]byte, length)

	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}

	if _, err := out.Write(header); err != nil {
		return err
	}

	if length == 0 {
		return nil
	}

	_, err := out.Write(masked)

	return err
}

// readFrame reads one frame.
func (w *WebSocket) readFrame() (opcode byte, fin bool, payload []byte, err error) {
	var first [2]byte

	if _, err = io.ReadFull(w.read, first[:]); err != nil {
		return 0, false, nil, err
	}

	fin = first[0]&0x80 != 0
	opcode = first[0] & 0x0F
	masked := first[1]&0x80 != 0
	length := int64(first[1] & 0x7F)

	switch length {
	case 126:
		var ext [2]byte

		if _, err = io.ReadFull(w.read, ext[:]); err != nil {
			return 0, false, nil, err
		}

		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte

		if _, err = io.ReadFull(w.read, ext[:]); err != nil {
			return 0, false, nil, err
		}

		length = int64(binary.BigEndian.Uint64(ext[:]))
	}

	if length < 0 || length > maxFrameBytes {
		return 0, false, nil, fmt.Errorf("websocket: frame of %d bytes is too large", length)
	}

	var mask [4]byte

	if masked {
		if _, err = io.ReadFull(w.read, mask[:]); err != nil {
			return 0, false, nil, err
		}
	}

	payload = make([]byte, length)

	if _, err = io.ReadFull(w.read, payload); err != nil {
		return 0, false, nil, err
	}

	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}

	return opcode, fin, payload, nil
}

// maxFrameBytes bounds one frame. A relay that announces a gigabyte is refused rather
// than allocated.
const maxFrameBytes = 8 << 20

// applyDeadline maps a context onto the socket's deadline.
func applyDeadline(conn net.Conn, ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	deadline, ok := ctx.Deadline()

	if !ok {
		return conn.SetDeadline(time.Time{})
	}

	return conn.SetDeadline(deadline)
}
