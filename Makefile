# Build and run the browser app.
#
# The Python engine builds and tests are separate (scripts/test.sh, Makefile
# would only get in their way); this file owns the wasm side only.
#
#   make          build both wasm instances and copy the loader
#   make serve    build, then run the development server
#   make verify   build, then prove the worker's greeting reaches the page
#   make test     native Go tests (packages that are not wasm-only)
#   make clean    remove generated wasm and the copied loader

TINYGO   ?= tinygo
GO       ?= go
TINYGOROOT := $(shell $(TINYGO) env TINYGOROOT)

WEB    := web
LOADER := $(WEB)/wasm_exec.js

# Everything the wasm modules are built from.
SOURCES := $(shell find pkg cmd -name '*.go' -not -name native_stub.go) go.mod go.sum

# Packages that build for the host: proto is pure Go, server is native.
NATIVE_PKGS := ./pkg/proto ./cmd/server

.PHONY: all wasm serve verify test fmt vet clean

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

serve: wasm
	$(GO) run ./cmd/server

verify: wasm
	./scripts/verify-hello.sh

test:
	$(GO) test $(NATIVE_PKGS)

fmt:
	$(GO) fmt ./pkg/... ./cmd/...

vet:
	$(GO) vet $(NATIVE_PKGS)

clean:
	rm -f $(WEB)/page.wasm $(WEB)/worker.wasm $(LOADER)
