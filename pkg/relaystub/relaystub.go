//go:build !js

// Package relaystub is a minimal Nostr relay for tests.
//
// The market commands are only worth testing against a relay that behaves like one: it
// stores what is sent, answers a subscription with the events that match and then EOSE, and
// forgets nothing within a test. Pointing tests at a public relay would make them depend on
// somebody else's server, its retention policy and its rate limits — and a test that needs the
// internet fails for reasons that have nothing to do with this code.
//
// What it is not: a production relay. There is no rate limiting, no authentication, no
// persistence, and it does not verify signatures (the client does that on the way in, which is
// where the protocol puts the obligation).
package relaystub

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"

	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/nostr"
)

// websocketGUID is the fixed string RFC 6455 mixes into the accept hash.
const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// The frame opcodes this stub understands.
const (
	opText  = 0x1
	opClose = 0x8
	opPing  = 0x9
	opPong  = 0xA
)

// Stub is one running relay.
type Stub struct {
	listener net.Listener
	// URL is the ws:// address to hand a relay client.
	URL string

	mu     sync.Mutex
	events []event.Event
	conns  map[net.Conn]bool

	closed   bool
	closeOne sync.Once
	wg       sync.WaitGroup
}

// New starts a relay on a loopback port.
//
// The caller closes it. A started-but-unclosed listening socket leaks a file descriptor per
// test, which shows up much later as "too many open files" in an unrelated suite.
func New() (*Stub, error) { return Listen("127.0.0.1:0") }

// Listen starts a relay on one address. An empty address means a loopback port chosen by the
// kernel, which is what a test wants; a fixed one is what a two-host run wants.
func Listen(address string) (*Stub, error) {
	listener, err := net.Listen("tcp", address)

	if err != nil {
		return nil, err
	}

	stub := &Stub{
		listener: listener,
		URL:      "ws://" + listener.Addr().String(),
		conns:    map[net.Conn]bool{},
	}

	stub.wg.Add(1)

	go stub.serve()

	return stub, nil
}

// Close stops the relay and every connection it is holding.
func (s *Stub) Close() {
	s.closeOne.Do(func() {
		s.mu.Lock()
		s.closed = true

		held := make([]net.Conn, 0, len(s.conns))

		for conn := range s.conns {
			held = append(held, conn)
		}

		s.mu.Unlock()

		_ = s.listener.Close()

		for _, conn := range held {
			_ = conn.Close()
		}
	})

	s.wg.Wait()
}

// Events returns what the relay is holding, in the order it accepted them.
func (s *Stub) Events() []event.Event {
	s.mu.Lock()
	defer s.mu.Unlock()

	stored := make([]event.Event, len(s.events))
	copy(stored, s.events)

	return stored
}

// Inject stores one event as though another client had published it.
//
// This is how a test plays the other participant without a second process: the relay is the
// only shared thing between two implementations anyway.
func (s *Stub) Inject(e event.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, existing := range s.events {
		if existing.ID == e.ID {
			return
		}
	}

	s.events = append(s.events, e)
}

// serve accepts connections until the listener is closed.
func (s *Stub) serve() {
	defer s.wg.Done()

	for {
		conn, err := s.listener.Accept()

		if err != nil {
			return
		}

		s.mu.Lock()

		if s.closed {
			s.mu.Unlock()
			_ = conn.Close()

			return
		}

		s.conns[conn] = true
		s.mu.Unlock()

		s.wg.Add(1)

		go func() {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.conns, conn)
				s.mu.Unlock()

				_ = conn.Close()
			}()

			s.handle(conn)
		}()
	}
}

// handle completes the upgrade handshake and then speaks frames.
func (s *Stub) handle(conn net.Conn) {
	reader := bufio.NewReader(conn)

	if _, err := handshake(reader, conn); err != nil {
		return
	}

	for {
		opcode, payload, err := readFrame(reader)

		if err != nil {
			return
		}

		switch opcode {
		case opClose:
			_ = writeFrame(conn, opClose, payload)

			return
		case opPing:
			if err := writeFrame(conn, opPong, payload); err != nil {
				return
			}
		case opText:
			if err := s.dispatch(conn, payload); err != nil {
				return
			}
		}
	}
}

// dispatch answers one client message.
func (s *Stub) dispatch(conn net.Conn, payload []byte) error {
	var frame []json.RawMessage

	if err := json.Unmarshal(payload, &frame); err != nil || len(frame) < 2 {
		return nil
	}

	var kind string

	if err := json.Unmarshal(frame[0], &kind); err != nil {
		return nil
	}

	switch kind {
	case "EVENT":
		return s.acceptEvent(conn, frame[1])
	case "REQ":
		return s.subscribe(conn, frame)
	case "CLOSE":
		// A subscription ends when the client says so; this stub answers everything at
		// once, so there is nothing left to stop.
		return nil
	}

	return nil
}

// acceptEvent stores one published event and acknowledges it.
func (s *Stub) acceptEvent(conn net.Conn, raw json.RawMessage) error {
	var published event.Event

	if err := json.Unmarshal(raw, &published); err != nil {
		return nil
	}

	s.Inject(published)

	acknowledgement, err := json.Marshal([]any{"OK", published.ID, true, ""})

	if err != nil {
		return err
	}

	return writeFrame(conn, opText, acknowledgement)
}

// subscribe answers one REQ with every matching event and then EOSE.
func (s *Stub) subscribe(conn net.Conn, frame []json.RawMessage) error {
	var subscription string

	if err := json.Unmarshal(frame[1], &subscription); err != nil {
		return nil
	}

	filters := make([]nostr.Filter, 0, len(frame)-2)

	for _, raw := range frame[2:] {
		filter, err := decodeFilter(raw)

		if err != nil {
			continue
		}

		filters = append(filters, filter)
	}

	sent := map[string]bool{}

	s.mu.Lock()
	held := make([]event.Event, len(s.events))
	copy(held, s.events)
	s.mu.Unlock()

	for _, candidate := range held {
		if sent[candidate.ID] {
			continue
		}

		// A subscription may carry several filters; an event is delivered once if it
		// matches any of them, which is what the client's dedupe expects.
		if !matchesAny(filters, candidate) {
			continue
		}

		sent[candidate.ID] = true

		message, err := json.Marshal([]any{"EVENT", subscription, candidate})

		if err != nil {
			return err
		}

		if err := writeFrame(conn, opText, message); err != nil {
			return err
		}
	}

	end, err := json.Marshal([]any{"EOSE", subscription})

	if err != nil {
		return err
	}

	return writeFrame(conn, opText, end)
}

// decodeFilter reads one subscription filter.
func decodeFilter(raw json.RawMessage) (nostr.Filter, error) {
	var object map[string]any

	if err := json.Unmarshal(raw, &object); err != nil {
		return nostr.Filter{}, err
	}

	filter := nostr.Filter{}

	if kinds, ok := object["kinds"].([]any); ok {
		for _, value := range kinds {
			if number, ok := value.(float64); ok {
				filter.Kinds = append(filter.Kinds, int(number))
			}
		}
	}

	strings := func(key string) []string {
		values, ok := object[key].([]any)

		if !ok {
			return nil
		}

		out := make([]string, 0, len(values))

		for _, value := range values {
			if text, ok := value.(string); ok {
				out = append(out, text)
			}
		}

		return out
	}

	filter.Authors = strings("authors")
	filter.Ids = strings("ids")
	filter.ETags = strings("#e")
	filter.PTags = strings("#p")
	filter.DTags = strings("#d")
	filter.Topics = strings("#t")

	if limit, ok := object["limit"].(float64); ok {
		filter.Limit = int(limit)
	}

	return filter, nil
}

// matchesAny reports whether an event satisfies at least one filter.
func matchesAny(filters []nostr.Filter, candidate event.Event) bool {
	if len(filters) == 0 {
		return true
	}

	for _, filter := range filters {
		if filter.Matches(candidate) {
			return true
		}
	}

	return false
}

// handshake answers the HTTP upgrade and returns the client's key.
func handshake(reader *bufio.Reader, conn net.Conn) (string, error) {
	key := ""

	for {
		line, err := reader.ReadString('\n')

		if err != nil {
			return "", err
		}

		trimmed := strings.TrimRight(line, "\r\n")

		if trimmed == "" {
			break
		}

		name, value, found := strings.Cut(trimmed, ":")

		if !found {
			continue
		}

		if strings.EqualFold(strings.TrimSpace(name), "Sec-WebSocket-Key") {
			key = strings.TrimSpace(value)
		}
	}

	if key == "" {
		return "", fmt.Errorf("relaystub: the client sent no Sec-WebSocket-Key")
	}

	digest := sha1.Sum([]byte(key + websocketGUID))

	accept := base64.StdEncoding.EncodeToString(digest[:])

	response := strings.Join([]string{
		"HTTP/1.1 101 Switching Protocols",
		"Upgrade: websocket",
		"Connection: Upgrade",
		"Sec-WebSocket-Accept: " + accept,
		"",
		"",
	}, "\r\n")

	if _, err := io.WriteString(conn, response); err != nil {
		return "", err
	}

	return key, nil
}

// readFrame reads one client frame. Client frames are always masked.
func readFrame(reader *bufio.Reader) (byte, []byte, error) {
	header := make([]byte, 2)

	if _, err := io.ReadFull(reader, header); err != nil {
		return 0, nil, err
	}

	opcode := header[0] & 0x0f
	masked := header[1]&0x80 != 0

	length := uint64(header[1] & 0x7f)

	switch length {
	case 126:
		extended := make([]byte, 2)

		if _, err := io.ReadFull(reader, extended); err != nil {
			return 0, nil, err
		}

		length = uint64(binary.BigEndian.Uint16(extended))
	case 127:
		extended := make([]byte, 8)

		if _, err := io.ReadFull(reader, extended); err != nil {
			return 0, nil, err
		}

		length = binary.BigEndian.Uint64(extended)
	}

	var mask []byte

	if masked {
		mask = make([]byte, 4)

		if _, err := io.ReadFull(reader, mask); err != nil {
			return 0, nil, err
		}
	}

	if length > 16<<20 {
		return 0, nil, fmt.Errorf("relaystub: frame of %d bytes is too large", length)
	}

	payload := make([]byte, length)

	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, nil, err
	}

	for index := range payload {
		if masked {
			payload[index] ^= mask[index%4]
		}
	}

	return opcode, payload, nil
}

// writeFrame writes one server frame. Server frames are never masked.
func writeFrame(writer io.Writer, opcode byte, payload []byte) error {
	frame := make([]byte, 0, len(payload)+10)
	frame = append(frame, 0x80|opcode)

	switch {
	case len(payload) < 126:
		frame = append(frame, byte(len(payload)))
	case len(payload) <= 0xffff:
		frame = append(frame, 126)
		frame = binary.BigEndian.AppendUint16(frame, uint16(len(payload)))
	default:
		frame = append(frame, 127)
		frame = binary.BigEndian.AppendUint64(frame, uint64(len(payload)))
	}

	frame = append(frame, payload...)

	_, err := writer.Write(frame)

	return err
}
