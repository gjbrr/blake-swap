//go:build !(js && wasm)

// The entry point is a wasm program: it uses syscall/js and cannot be built
// for a native target. This stub keeps `go build ./...`, `go vet ./...` and
// `go list ./...` working on a development host; the Makefile builds the real
// program with tinygo -target=wasm.
package main

func main() {}
