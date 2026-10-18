//go:build !js

package platform

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// relay is the server half of one WebSocket connection: enough of RFC 6455 to accept a
// client and to exchange frames with it.
type relay struct {
	address  string
	accepted chan *peer
	failure  chan error
}

// peer is the accepted side after the handshake.
type peer struct {
	conn net.Conn
	read *bufio.Reader
}

// startRelay listens, accepts one connection and completes the handshake. A non-empty
// reject answers with that status and closes instead, which is how a relay that does
// not speak WebSocket looks to a client.
func startRelay(t *testing.T, reject string) *relay {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")

	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = listener.Close() })

	r := &relay{
		address:  listener.Addr().String(),
		accepted: make(chan *peer, 1),
		failure:  make(chan error, 1),
	}

	go func() {
		conn, err := listener.Accept()

		if err != nil {
			r.failure <- err

			return
		}

		p := &peer{conn: conn, read: bufio.NewReader(conn)}

		key, err := p.readHandshake()

		if err != nil {
			r.failure <- err
			_ = conn.Close()

			return
		}

		if reject != "" {
			_, _ = io.WriteString(conn, "HTTP/1.1 "+reject+"\r\n\r\n")
			_ = conn.Close()

			return
		}

		answer := strings.Join([]string{
			"HTTP/1.1 101 Switching Protocols",
			"Upgrade: websocket",
			"Connection: Upgrade",
			"Sec-WebSocket-Accept: " + acceptKey(key),
			"",
			"",
		}, "\r\n")

		if _, err := io.WriteString(conn, answer); err != nil {
			r.failure <- err
			_ = conn.Close()

			return
		}

		r.accepted <- p
	}()

	return r
}

// url is where the relay is listening.
func (r *relay) url() string { return "ws://" + r.address + "/relay" }

// client dials the relay and returns the accepted peer.
func (r *relay) client(t *testing.T) (*WebSocket, *peer) {
	t.Helper()

	ws, err := DialWebSocket(context.Background(), r.url())

	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = ws.Close() })

	select {
	case p := <-r.accepted:
		t.Cleanup(func() { _ = p.conn.Close() })

		return ws, p
	case err := <-r.failure:
		t.Fatalf("relay failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the relay never accepted the connection")
	}

	return nil, nil
}

// readHandshake reads the upgrade request and returns its nonce.
func (p *peer) readHandshake() (string, error) {
	key := ""

	for {
		line, err := p.read.ReadString('\n')

		if err != nil {
			return "", err
		}

		line = strings.TrimRight(line, "\r\n")

		if line == "" {
			return key, nil
		}

		name, value, found := strings.Cut(line, ":")

		if found && strings.EqualFold(strings.TrimSpace(name), "Sec-WebSocket-Key") {
			key = strings.TrimSpace(value)
		}
	}
}

// readFrame reads one frame the client sent, unmasking it.
func (p *peer) readFrame(t *testing.T) (byte, []byte) {
	t.Helper()

	var first [2]byte

	if _, err := io.ReadFull(p.read, first[:]); err != nil {
		t.Fatalf("reading a frame: %v", err)
	}

	opcode := first[0] & 0x0F
	length := int64(first[1] & 0x7F)

	if first[1]&0x80 == 0 {
		t.Fatal("a client frame arrived unmasked")
	}

	switch length {
	case 126:
		var ext [2]byte

		if _, err := io.ReadFull(p.read, ext[:]); err != nil {
			t.Fatalf("reading an extended length: %v", err)
		}

		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte

		if _, err := io.ReadFull(p.read, ext[:]); err != nil {
			t.Fatalf("reading an extended length: %v", err)
		}

		length = int64(binary.BigEndian.Uint64(ext[:]))
	}

	var mask [4]byte

	if _, err := io.ReadFull(p.read, mask[:]); err != nil {
		t.Fatalf("reading a mask: %v", err)
	}

	payload := make([]byte, length)

	if _, err := io.ReadFull(p.read, payload); err != nil {
		t.Fatalf("reading a payload: %v", err)
	}

	for i := range payload {
		payload[i] ^= mask[i%4]
	}

	return opcode, payload
}

// writeFrame writes one unmasked server frame.
func (p *peer) writeFrame(t *testing.T, opcode byte, payload []byte) {
	t.Helper()

	header := []byte{0x80 | opcode}

	switch length := len(payload); {
	case length < 126:
		header = append(header, byte(length))
	case length <= 0xFFFF:
		header = append(header, 126, byte(length>>8), byte(length))
	default:
		var wide [8]byte

		binary.BigEndian.PutUint64(wide[:], uint64(length))
		header = append(header, 127)
		header = append(header, wide[:]...)
	}

	if _, err := p.conn.Write(append(header, payload...)); err != nil {
		t.Fatalf("writing a frame: %v", err)
	}
}

// receive reads one frame from the client with a deadline, so a client that never
// answers fails the test instead of hanging it.
func receive(t *testing.T, ws *WebSocket) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	payload, err := ws.Receive(ctx)

	if err != nil {
		t.Fatalf("receive: %v", err)
	}

	return payload
}

// TestWebSocketRoundTrip: a text frame in each direction arrives intact.
func TestWebSocketRoundTrip(t *testing.T) {
	r := startRelay(t, "")
	ws, peer := r.client(t)

	if err := ws.Send(context.Background(), "hello relay"); err != nil {
		t.Fatal(err)
	}

	opcode, payload := peer.readFrame(t)

	if opcode != opText || string(payload) != "hello relay" {
		t.Fatalf("frame = opcode %#x, payload %q", opcode, payload)
	}

	peer.writeFrame(t, opText, []byte("hello client"))

	if got := receive(t, ws); got != "hello client" {
		t.Fatalf("receive = %q", got)
	}
}

// TestWebSocketRefusesANonUpgradeAnswer: a server that answers 200 is not a relay, and
// the error must say so at dial time rather than at the first send.
func TestWebSocketRefusesANonUpgradeAnswer(t *testing.T) {
	r := startRelay(t, "200 OK")

	_, err := DialWebSocket(context.Background(), r.url())

	if err == nil || !strings.Contains(err.Error(), "refused the upgrade") {
		t.Fatalf("err = %v", err)
	}
}

// TestWebSocketAnswersPing: a ping is answered without being surfaced, or the relay
// times the connection out.
func TestWebSocketAnswersPing(t *testing.T) {
	r := startRelay(t, "")
	ws, peer := r.client(t)

	peer.writeFrame(t, opPing, []byte("keepalive"))
	peer.writeFrame(t, opText, []byte("after the ping"))

	if got := receive(t, ws); got != "after the ping" {
		t.Fatalf("receive = %q, want the text frame after the ping", got)
	}

	opcode, payload := peer.readFrame(t)

	if opcode != opPong || string(payload) != "keepalive" {
		t.Fatalf("answer = opcode %#x, payload %q", opcode, payload)
	}
}

// TestWebSocketCloseReadsAsEOF: a closed subscription is io.EOF, which is what tells
// the relay client to stop reading.
func TestWebSocketCloseReadsAsEOF(t *testing.T) {
	r := startRelay(t, "")
	ws, peer := r.client(t)

	peer.writeFrame(t, opClose, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := ws.Receive(ctx); err != io.EOF {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}

// TestWebSocketSendMasksFrames: an unmasked client frame is a protocol violation, and
// the relay-side reader fails the test if it sees one.
func TestWebSocketSendMasksFrames(t *testing.T) {
	r := startRelay(t, "")
	ws, peer := r.client(t)

	if err := ws.Send(context.Background(), "masked"); err != nil {
		t.Fatal(err)
	}

	if _, payload := peer.readFrame(t); string(payload) != "masked" {
		t.Fatalf("payload = %q", payload)
	}
}

// TestWebSocketRefusesABadScheme: only ws:// and wss:// are relay URLs.
func TestWebSocketRefusesABadScheme(t *testing.T) {
	if _, err := DialWebSocket(context.Background(), "http://relay.example"); err == nil {
		t.Fatal("an http URL was accepted as a relay")
	}
}

// TestWebSocketLargeFrameRoundTrips: a payload above the 7-bit length goes through the
// extended length path in both directions.
func TestWebSocketLargeFrameRoundTrips(t *testing.T) {
	r := startRelay(t, "")
	ws, peer := r.client(t)

	big := strings.Repeat("x", 70_000)

	if err := ws.Send(context.Background(), big); err != nil {
		t.Fatal(err)
	}

	if _, payload := peer.readFrame(t); len(payload) != len(big) {
		t.Fatalf("payload = %d bytes, want %d", len(payload), len(big))
	}

	peer.writeFrame(t, opText, []byte(big))

	if got := receive(t, ws); len(got) != len(big) {
		t.Fatalf("receive = %d bytes, want %d", len(got), len(big))
	}
}

// TestAcceptKeyIsTheRFCExample pins the handshake hash to the worked example in RFC
// 6455, so a refactor cannot quietly change the proof a relay checks.
func TestAcceptKeyIsTheRFCExample(t *testing.T) {
	const want = "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="

	if got := acceptKey("dGhlIHNhbXBsZSBub25jZQ=="); got != want {
		t.Fatalf("acceptKey = %q, want %q", got, want)
	}
}
