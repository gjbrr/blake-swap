//go:build js && wasm

package platform

import (
	"context"
	"fmt"
	"sync"
	"syscall/js"
	"time"
)

// JSClock is the browser's clock.
type JSClock struct{}

// Now implements Clock.
func (JSClock) Now() time.Time {
	return time.UnixMilli(int64(js.Global().Get("Date").New().Call("getTime").Int()))
}

// JSConsole is the browser's console.
type JSConsole struct {
	// Prefix is prepended to every line, so a worker's output is distinguishable
	// from the page's.
	Prefix string
}

// Log implements Console.
func (c JSConsole) Log(message string) {
	js.Global().Get("console").Call("log", c.Prefix+message)
}

// IndexedDB is a KV over the browser's IndexedDB.
//
// IndexedDB is asynchronous, so each operation here is a promise that is resolved by
// a JavaScript callback. The Go side waits on that callback with a channel: a
// synchronous-looking KV is possible only because goroutines make the wait cheap, and
// pretending otherwise would mean either blocking the JavaScript thread or inventing a
// second, hand-rolled event loop.
type IndexedDB struct {
	// Database and Store name the database and object store.
	Database string
	Store    string

	once sync.Once
	db   js.Value
	err  error
}

// NewIndexedDB returns a handle; the database is opened on first use.
func NewIndexedDB(database, storeName string) *IndexedDB {
	return &IndexedDB{Database: database, Store: storeName}
}

// open opens the database once.
func (i *IndexedDB) open(ctx context.Context) (js.Value, error) {
	i.once.Do(func() {
		request := js.Global().Get("indexedDB").Call("open", i.Database, 1)

		// Create the object store on first upgrade.
		request.Set("onupgradeneeded", js.FuncOf(func(this js.Value, args []js.Value) any {
			event := args[0]
			database := event.Get("target").Get("result")

			if !database.Get("objectStoreNames").Call("contains", i.Store).Bool() {
				database.Call("createObjectStore", i.Store)
			}

			return nil
		}))

		done := make(chan struct{})

		request.Set("onsuccess", js.FuncOf(func(this js.Value, args []js.Value) any {
			i.db = args[0].Get("target").Get("result")
			close(done)

			return nil
		}))

		request.Set("onerror", js.FuncOf(func(this js.Value, args []js.Value) any {
			i.err = fmt.Errorf("indexeddb: %v", args[0].Get("target").Get("error").Get("message").String())
			close(done)

			return nil
		}))

		<-done
	})

	return i.db, i.err
}

// store runs one transaction against the object store.
//
// `operation` receives the store and returns the request whose result is wanted; a nil
// result means the caller does not need one.
func (i *IndexedDB) store(
	ctx context.Context, mode string, operation func(store js.Value) js.Value,
) (js.Value, error) {
	db, err := i.open(ctx)

	if err != nil {
		return js.Undefined(), err
	}

	transaction := db.Call("transaction", i.Store, mode)
	store := transaction.Call("objectStore", i.Store)

	request := operation(store)

	if request.IsUndefined() || request.IsNull() {
		return js.Undefined(), nil
	}

	done := make(chan struct{})

	var (
		result js.Value
		failed error
	)

	request.Set("onsuccess", js.FuncOf(func(this js.Value, args []js.Value) any {
		result = args[0].Get("target").Get("result")
		close(done)

		return nil
	}))

	request.Set("onerror", js.FuncOf(func(this js.Value, args []js.Value) any {
		failed = fmt.Errorf("indexeddb: %v", args[0].Get("target").Get("error").Get("message").String())
		close(done)

		return nil
	}))

	<-done

	return result, failed
}

// Get implements KV.
func (i *IndexedDB) Get(ctx context.Context, key string) ([]byte, bool, error) {
	result, err := i.store(ctx, "readonly", func(store js.Value) js.Value {
		return store.Call("get", key)
	})

	if err != nil {
		return nil, false, err
	}

	if result.IsUndefined() || result.IsNull() {
		return nil, false, nil
	}

	buffer := js.Global().Get("Uint8Array").New(result)

	decoded := make([]byte, buffer.Length())
	js.CopyBytesToGo(decoded, buffer)

	return decoded, true, nil
}

// Put implements KV.
func (i *IndexedDB) Put(ctx context.Context, key string, value []byte) error {
	buffer := js.Global().Get("Uint8Array").New(len(value))
	js.CopyBytesToJS(buffer, value)

	_, err := i.store(ctx, "readwrite", func(store js.Value) js.Value {
		return store.Call("put", buffer, key)
	})

	return err
}

// Delete implements KV.
func (i *IndexedDB) Delete(ctx context.Context, key string) error {
	_, err := i.store(ctx, "readwrite", func(store js.Value) js.Value {
		return store.Call("delete", key)
	})

	return err
}

// Keys implements KV.
func (i *IndexedDB) Keys(ctx context.Context, prefix string) ([]string, error) {
	result, err := i.store(ctx, "readonly", func(store js.Value) js.Value {
		return store.Call("getAllKeys")
	})

	if err != nil {
		return nil, err
	}

	if result.IsUndefined() || result.IsNull() {
		return nil, nil
	}

	length := result.Length()
	keys := make([]string, 0, length)

	for index := 0; index < length; index++ {
		key := result.Index(index).String()

		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			keys = append(keys, key)
		}
	}

	sortStrings(keys)

	return keys, nil
}

// WebSocket is a Conn over the browser's WebSocket.
//
// It is the relay seam: the nostr client speaks frames to this, and the browser
// provides the socket. Messages arrive as events, so reads are queued on a channel that
// the Go side drains.
type WebSocket struct {
	socket  js.Value
	inbound chan []byte
	closed  chan struct{}
	once    sync.Once
	err     error
}

// DialWebSocket opens one connection.
//
// The connection is awaited before returning, because a caller that gets a Conn has to
// be able to send immediately: a relay that is unreachable must fail here, where the
// error names the relay, rather than as a mysterious send failure later.
func DialWebSocket(ctx context.Context, url string) (*WebSocket, error) {
	socket := js.Global().Get("WebSocket").New(url)

	conn := &WebSocket{
		socket:  socket,
		inbound: make(chan []byte, 64),
		closed:  make(chan struct{}),
	}

	opened := make(chan struct{})

	socket.Set("onopen", js.FuncOf(func(this js.Value, args []js.Value) any {
		close(opened)

		return nil
	}))

	socket.Set("onmessage", js.FuncOf(func(this js.Value, args []js.Value) any {
		text := args[0].Get("data").String()

		select {
		case conn.inbound <- []byte(text):
		case <-conn.closed:
		default:
			// A full queue means the consumer is not reading; dropping the frame is
			// reported rather than blocking the JavaScript thread.
			conn.fail(fmt.Errorf("websocket: inbound queue is full"))
		}

		return nil
	}))

	socket.Set("onclose", js.FuncOf(func(this js.Value, args []js.Value) any {
		conn.fail(fmt.Errorf("websocket: closed by the relay"))
		conn.shutdown()

		return nil
	}))

	socket.Set("onerror", js.FuncOf(func(this js.Value, args []js.Value) any {
		conn.fail(fmt.Errorf("websocket: the relay reported an error"))
		conn.shutdown()

		return nil
	}))

	select {
	case <-opened:
		return conn, nil
	case <-conn.closed:
		return nil, conn.failure()
	case <-ctx.Done():
		socket.Call("close")

		return nil, ctx.Err()
	}
}

// fail records the first failure.
func (w *WebSocket) fail(err error) {
	if w.err == nil {
		w.err = err
	}
}

// failure returns the recorded error, or a generic one.
func (w *WebSocket) failure() error {
	if w.err != nil {
		return w.err
	}

	return fmt.Errorf("websocket: closed")
}

// shutdown closes the inbound channel exactly once.
func (w *WebSocket) shutdown() {
	w.once.Do(func() { close(w.closed) })
}

// Send implements nostr.Conn.
func (w *WebSocket) Send(ctx context.Context, payload string) error {
	select {
	case <-w.closed:
		return w.failure()
	default:
	}

	w.socket.Call("send", payload)

	return nil
}

// Receive implements nostr.Conn.
func (w *WebSocket) Receive(ctx context.Context) (string, error) {
	select {
	case frame := <-w.inbound:
		return string(frame), nil
	case <-w.closed:
		return "", w.failure()
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Close implements nostr.Conn.
func (w *WebSocket) Close() error {
	w.socket.Call("close")
	w.shutdown()

	return nil
}

// Fetch is the browser implementation of rpc.Transport: a POST over fetch.
//
// The node credentials never enter the browser: this talks to the app server's proxy,
// which holds them. That is why there is no user or password here.
type Fetch struct {
	// URL is the proxy endpoint.
	URL string
	// Timeout bounds one request.
	Timeout time.Duration
}

// Do implements rpc.Transport.
func (f *Fetch) Do(ctx context.Context, body []byte) ([]byte, error) {
	if f.URL == "" {
		return nil, fmt.Errorf("fetch: no proxy URL configured")
	}

	controller := js.Global().Get("AbortController").New()
	signal := controller.Get("signal")

	timeout := f.Timeout

	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	cancel := time.AfterFunc(timeout, func() { controller.Call("abort") })
	defer cancel.Stop()

	buffer := js.Global().Get("Uint8Array").New(len(body))
	js.CopyBytesToJS(buffer, body)

	options := js.Global().Get("Object").New()
	options.Set("method", "POST")
	options.Set("body", buffer)
	options.Set("signal", signal)
	options.Set("headers", js.Global().Get("Object").New())
	options.Get("headers").Set("Content-Type", "application/json")

	response := make(chan js.Value, 1)
	failure := make(chan error, 1)

	js.Global().Call("fetch", f.URL, options).Call("then",
		js.FuncOf(func(this js.Value, args []js.Value) any {
			response <- args[0]

			return nil
		}),
	).Call("catch", js.FuncOf(func(this js.Value, args []js.Value) any {
		failure <- fmt.Errorf("fetch: %v", args[0].Call("toString").String())

		return nil
	}))

	select {
	case err := <-failure:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-response:
		return readFetchBody(result)
	}
}

// readFetchBody reads a fetch response body into bytes.
func readFetchBody(response js.Value) ([]byte, error) {
	done := make(chan []byte, 1)
	failure := make(chan error, 1)

	response.Call("arrayBuffer").Call("then",
		js.FuncOf(func(this js.Value, args []js.Value) any {
			buffer := js.Global().Get("Uint8Array").New(args[0])

			decoded := make([]byte, buffer.Length())
			js.CopyBytesToGo(decoded, buffer)
			done <- decoded

			return nil
		}),
	).Call("catch", js.FuncOf(func(this js.Value, args []js.Value) any {
		failure <- fmt.Errorf("fetch: %v", args[0].Call("toString").String())

		return nil
	}))

	select {
	case body := <-done:
		return body, nil
	case err := <-failure:
		return nil, err
	}
}

// readRandom fills p from WebCrypto.
//
// A wasm build's crypto/rand is routed through the runtime's entropy source, which is
// not a guarantee this code can make about the browser; WebCrypto is. The key that
// protects a swap's secret is derived from a passphrase, but the nonces that make two
// seals differ come from here.
func readRandom(p []byte) (int, error) {
	buffer := js.Global().Get("Uint8Array").New(len(p))

	js.Global().Get("crypto").Call("getRandomValues", buffer)

	js.CopyBytesToGo(p, buffer)

	return len(p), nil
}
