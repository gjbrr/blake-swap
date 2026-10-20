//go:build !js

package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestHTTPPostsJSONRPC proves the host transport puts the body on the wire unchanged
// and hands the answer back as-is.
func TestHTTPPostsJSONRPC(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}

		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content type = %q", got)
		}

		body := make([]byte, r.ContentLength)

		if _, err := r.Body.Read(body); err != nil && err.Error() != "EOF" {
			t.Errorf("read body: %v", err)
		}

		var request map[string]any

		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("body is not JSON: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":7,"error":null,"id":"x"}`))
	}))
	defer server.Close()

	transport := &HTTP{URL: server.URL, Timeout: 5 * time.Second}

	body, err := transport.Do(context.Background(), []byte(`{"method":"getblockcount","params":[]}`))

	if err != nil {
		t.Fatal(err)
	}

	if string(body) != `{"result":7,"error":null,"id":"x"}` {
		t.Fatalf("body = %s", body)
	}
}

// TestHTTPForwardsErrorBodies: a JSON-RPC error is reported in the body with a 4xx or
// 5xx status, and the transport's job is to hand the body over rather than decide what it
// means — otherwise the error's code and message would be replaced by a transport error.
func TestHTTPForwardsErrorBodies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"result":null,"error":{"code":-5,"message":"No such transaction"},"id":"x"}`))
	}))
	defer server.Close()

	transport := &HTTP{URL: server.URL}

	body, err := transport.Do(context.Background(), []byte(`{}`))

	if err != nil {
		t.Fatalf("an error body should be returned, not converted: %v", err)
	}

	var response struct {
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}

	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}

	if response.Error.Code != -5 {
		t.Fatalf("code = %d", response.Error.Code)
	}
}

// TestHTTPEmptyErrorIsAnError: an empty error body has no code to report, so it becomes
// a transport error naming the status.
func TestHTTPEmptyErrorIsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	transport := &HTTP{URL: server.URL}

	if _, err := transport.Do(context.Background(), []byte(`{}`)); err == nil {
		t.Fatal("an empty 502 was accepted")
	}
}

// TestHTTPBasicAuth: the host transport carries node credentials, which is the whole
// reason a browser cannot use it directly.
func TestHTTPBasicAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()

		if !ok || user != "__cookie__" || password != "secret" {
			t.Errorf("auth = %q/%q ok=%v", user, password, ok)
		}

		_, _ = w.Write([]byte(`{"result":1}`))
	}))
	defer server.Close()

	transport := &HTTP{URL: server.URL, User: "__cookie__", Password: "secret"}

	if _, err := transport.Do(context.Background(), []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
}

// TestMemoryKV covers the KV contract both implementations must share.
func TestMemoryKV(t *testing.T) {
	ctx := context.Background()
	kv := NewMemoryKV()

	if _, found, err := kv.Get(ctx, "missing"); err != nil || found {
		t.Fatalf("found=%v err=%v", found, err)
	}

	if err := kv.Put(ctx, "a/one", []byte("1")); err != nil {
		t.Fatal(err)
	}

	if err := kv.Put(ctx, "a/two", []byte("2")); err != nil {
		t.Fatal(err)
	}

	if err := kv.Put(ctx, "b/three", []byte("3")); err != nil {
		t.Fatal(err)
	}

	keys, err := kv.Keys(ctx, "a/")

	if err != nil {
		t.Fatal(err)
	}

	if len(keys) != 2 || keys[0] != "a/one" || keys[1] != "a/two" {
		t.Fatalf("keys = %v", keys)
	}

	// A write copies, so a caller reusing its buffer cannot corrupt the store.
	buffer := []byte("x")
	_ = kv.Put(ctx, "c", buffer)
	buffer[0] = 'y'

	value, _, _ := kv.Get(ctx, "c")

	if string(value) != "x" {
		t.Fatalf("value = %q", value)
	}

	if err := kv.Delete(ctx, "a/one"); err != nil {
		t.Fatal(err)
	}

	if keys, _ = kv.Keys(ctx, "a/"); len(keys) != 1 || keys[0] != "a/two" {
		t.Fatalf("keys after delete = %v", keys)
	}
}

// TestMemoryKVReplaceAll: the replace makes the store hold exactly the new entries.
//
// It is the primitive a vault restore needs, so what it must do is total: keys that were
// there and are not in the replacement are gone, values are copied, and an empty
// replacement is an empty store rather than a no-op.
func TestMemoryKVReplaceAll(t *testing.T) {
	ctx := context.Background()
	kv := NewMemoryKV()

	for _, key := range []string{"vault/header", "record/one", "secret/two"} {
		if err := kv.Put(ctx, key, []byte("old")); err != nil {
			t.Fatal(err)
		}
	}

	buffer := []byte("new")
	replacement := map[string][]byte{
		"vault/header": buffer,
		"secret/two":   []byte("kept"),
	}

	if err := kv.ReplaceAll(ctx, replacement); err != nil {
		t.Fatal(err)
	}

	// The caller's buffer is copied, exactly as Put copies one.
	buffer[0] = 'x'

	value, found, err := kv.Get(ctx, "vault/header")

	if err != nil || !found || string(value) != "new" {
		t.Fatalf("header = %q, %v, %v", value, found, err)
	}

	if _, found, _ := kv.Get(ctx, "record/one"); found {
		t.Fatal("a key that was not in the replacement survived it")
	}

	keys, err := kv.Keys(ctx, "")

	if err != nil {
		t.Fatal(err)
	}

	if len(keys) != 2 || keys[0] != "secret/two" || keys[1] != "vault/header" {
		t.Fatalf("keys = %v", keys)
	}

	if err := kv.ReplaceAll(ctx, map[string][]byte{}); err != nil {
		t.Fatal(err)
	}

	if keys, _ = kv.Keys(ctx, ""); len(keys) != 0 {
		t.Fatalf("an empty replacement left %v", keys)
	}
}
