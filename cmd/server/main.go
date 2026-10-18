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
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8787", "listen address")
	dir := flag.String("dir", "web", "directory holding index.html and the wasm modules")
	open := flag.Bool("open", false, "open the application in the default browser")
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

	server := &http.Server{
		Handler: handler(root),
	}

	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		fail("serve: %v", err)
	}
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
