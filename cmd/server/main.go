// Command server serves the wasm application: the page, the service worker,
// their wasm modules, and the TinyGo loader they share.
//
// It is a development server, deliberately small. It serves files from disk so
// that `make wasm` followed by a refresh is the whole edit loop, and it sets
// the two headers that matter for wasm: application/wasm so streaming
// instantiation works, and no-store so a rebuilt module is never served from
// cache.
package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/gjbrr/blake-swap/pkg/proxy"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8787", "listen address")
	dir := flag.String("dir", "web", "directory holding index.html and the wasm modules")
	open := flag.Bool("open", false, "open the application in the default browser")
	btcRPC := flag.String("btc-rpc", "", "Bitcoin node JSON-RPC URL, such as http://127.0.0.1:18443")
	blakeRPC := flag.String("blake-rpc", "", "BLAKE node JSON-RPC URL")
	btcAuth := flag.String("btc-auth", "", "Bitcoin node credentials as user:password")
	blakeAuth := flag.String("blake-auth", "", "BLAKE node credentials as user:password")
	btcCookie := flag.String("btc-cookie", "", "file holding the Bitcoin node's RPC cookie")
	blakeCookie := flag.String("blake-cookie", "", "file holding the BLAKE node's RPC cookie")
	flag.Parse()

	root, err := filepath.Abs(*dir)

	if err != nil {
		fail("resolve %s: %v", *dir, err)
	}

	if _, err := os.Stat(filepath.Join(root, "index.html")); err != nil {
		fail("%s does not look like the app directory: %v", root, err)
	}

	listener, err := net.Listen("tcp", *addr)

	if err != nil {
		fail("listen on %s: %v", *addr, err)
	}

	url := fmt.Sprintf("http://%s/", listener.Addr().String())

	fmt.Printf("blake-swap wasm app\n  root: %s\n  url:  %s\n\n", root, url)

	if *open {
		if err := openBrowser(url); err != nil {
			fmt.Fprintf(os.Stderr, "could not open a browser: %v\n", err)
		}
	}

	appHandler := handler(root)

	rpcProxy, err := buildProxy(proxyConfig{
		btcRPC: *btcRPC, blakeRPC: *blakeRPC,
		btcAuth: *btcAuth, blakeAuth: *blakeAuth,
		btcCookie: *btcCookie, blakeCookie: *blakeCookie,
	})

	if err != nil {
		fail("%v", err)
	}

	mux := http.NewServeMux()

	if rpcProxy != nil {
		mux.Handle("/rpc/", rpcProxy)

		fmt.Printf("  node proxy: /rpc/<chain>[/wallet/<name>]\n")

		defer rpcProxy.Close()
	}

	mux.Handle("/", appHandler)

	server := &http.Server{
		Handler: mux,
	}

	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		fail("serve: %v", err)
	}
}

// proxyConfig is the flag surface, kept as a struct so the builder is testable.
type proxyConfig struct {
	btcRPC, blakeRPC       string
	btcAuth, blakeAuth     string
	btcCookie, blakeCookie string
}

// buildProxy configures the node proxy, or returns nil when no node is configured.
//
// A server with no node is a perfectly good static host: it serves the app and the
// browser reports that the chain is unreachable. Refusing to start would make the flag
// set mandatory for a feature the user may not want yet.
func buildProxy(cfg proxyConfig) (*proxy.Proxy, error) {
	chains := map[string]proxy.Endpoint{}

	add := func(name, url, auth, cookie string) error {
		if url == "" {
			return nil
		}

		endpoint := proxy.Endpoint{URL: url}

		switch {
		case auth != "":
			user, password, ok := strings.Cut(auth, ":")

			if !ok {
				return fmt.Errorf("%s auth must be user:password", name)
			}

			endpoint.User, endpoint.Password = user, password
		case cookie != "":
			raw, err := os.ReadFile(cookie)

			if err != nil {
				return fmt.Errorf("%s cookie: %w", name, err)
			}

			user, password, ok := strings.Cut(strings.TrimSpace(string(raw)), ":")

			if !ok {
				return fmt.Errorf("%s cookie is malformed", name)
			}

			endpoint.User, endpoint.Password = user, password
		}

		chains[name] = endpoint

		return nil
	}

	if err := add("btc", cfg.btcRPC, cfg.btcAuth, cfg.btcCookie); err != nil {
		return nil, err
	}

	if err := add("blake", cfg.blakeRPC, cfg.blakeAuth, cfg.blakeCookie); err != nil {
		return nil, err
	}

	if len(chains) == 0 {
		return nil, nil
	}

	return proxy.New(proxy.Config{Chains: chains})
}

// handler serves the app directory with the headers wasm needs.
func handler(root string) http.Handler {
	files := http.FileServer(http.Dir(root))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		//
		// A rebuilt module is the common case during development, so
		// never let the browser reuse one. The modules are local files
		// and the server is not meant for production traffic.
		//
		w.Header().Set("Cache-Control", "no-store")

		switch strings.ToLower(filepath.Ext(r.URL.Path)) {
		case ".wasm":
			w.Header().Set("Content-Type", "application/wasm")
		case ".js":
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		case ".html", "":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		}

		files.ServeHTTP(w, r)
	})
}

// openBrowser is best-effort: a headless host has no browser, and that is not
// a failure of the server.
func openBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		for _, cmd := range []string{"xdg-open", "sensible-browser", "x-www-browser"} {
			if path, err := exec.LookPath(cmd); err == nil {
				return exec.Command(path, url).Start()
			}
		}
	}

	return fmt.Errorf("no browser launcher found")
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "server: "+format+"\n", args...)
	os.Exit(1)
}
