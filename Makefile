# Build and run the browser app.
#
# The Python engine builds and tests are separate (scripts/test.sh, Makefile
# would only get in their way); this file owns the wasm side only.
#
#   make          build both wasm instances and copy the loader
#   make server   build the development server binary (also TinyGo)
#   make serve    build everything, then run the server
#   make verify   build, then prove the worker's greeting reaches the page
#   make test     native Go tests (packages that are not wasm-only)
#   make clean    remove generated wasm, the server binary and the loader

TINYGO   ?= tinygo
GO       ?= go
TINYGOROOT := $(shell $(TINYGO) env TINYGOROOT)

WEB    := web
LOADER := $(WEB)/wasm_exec.js
BIN    := bin
SERVER := $(BIN)/blake-swap-app

# Everything the wasm modules are built from.
SOURCES := $(shell find pkg cmd -name '*.go' -not -name native_stub.go) go.mod go.sum

# Packages that build for the host. Everything except the browser seams and
# the wasm entry points is host-buildable, which is what makes the value layer
# testable without a browser.
NATIVE_PKGS := ./pkg/... ./internal/... ./cmd/server

# Packages whose tests also run under TinyGo wasm. They are the ones the
# browser client will link, so a host-only pass would not be enough.
WASM_TEST_PKGS := ./pkg/decimal ./pkg/amount ./pkg/fees ./pkg/timeout ./pkg/state ./pkg/model ./pkg/wire ./pkg/reconcile ./pkg/htlc ./pkg/psbt ./pkg/market ./pkg/keys ./pkg/nip44 ./pkg/event ./pkg/vault ./pkg/rpc ./pkg/contract ./pkg/observe ./pkg/fund ./pkg/settle ./pkg/store ./pkg/nostr ./pkg/coord ./pkg/marketflow ./pkg/client ./pkg/platform ./pkg/worker ./pkg/swap

.PHONY: all wasm server serve verify test wasmtest fmt vet clean

all: wasm

wasm: $(WEB)/page.wasm $(WEB)/worker.wasm

# TinyGo ships the loader; copy it so the app is self-contained on disk.
$(LOADER): $(TINYGOROOT)/targets/wasm_exec.js
	cp $< $@

# The two instances are separate modules with separate mains. They share the
# loader and must be rebuilt together whenever the shared packages change.
$(WEB)/page.wasm: $(SOURCES) $(LOADER)
	$(TINYGO) build -target=wasm -o $@ ./cmd/webpage

$(WEB)/worker.wasm: $(SOURCES) $(LOADER)
	$(TINYGO) build -target=wasm -o $@ ./cmd/webworker

# The server is TinyGo too: one toolchain builds the whole app, and the
# binary starts instantly instead of compiling on first request.
server: $(SERVER)

$(SERVER): $(SOURCES) $(LOADER)
	@mkdir -p $(BIN)
	$(TINYGO) build -o $@ ./cmd/server

serve: wasm server
	$(SERVER) -addr 127.0.0.1:8787 -dir $(WEB)

verify: wasm
	./scripts/verify-hello.sh

test:
	$(GO) test $(NATIVE_PKGS)

# Run the value-layer tests compiled to wasm. This is how a package that
# accidentally depends on something TinyGo lacks gets caught here rather than
# at browser runtime.
wasmtest:
	$(TINYGO) test -target=wasm $(WASM_TEST_PKGS)

fmt:
	$(GO) fmt ./pkg/... ./cmd/...

vet:
	$(GO) vet $(NATIVE_PKGS)

clean:
	rm -f $(WEB)/page.wasm $(WEB)/worker.wasm $(LOADER)
	rm -rf $(BIN)
