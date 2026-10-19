package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/gjbrr/blake-swap/pkg/marketops"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/store"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// stateFiles is the CLI's durable side for the market operations: the same selection records
// and match bindings under the state directory that the market commands have always written.
//
// The path shape is not new. The names the operations use ("selections/…", "bindings/…") map
// onto the directories the CLI already had, so a record written by an earlier build is still
// found, and a record written now is still found by one.
type stateFiles struct {
	root string
}

// marketPath maps one operation name onto the CLI's directories.
func (f stateFiles) marketPath(name string) string {
	dir, rest, found := cutSlash(name)

	if !found {
		return filepath.Join(f.root, name)
	}

	switch dir {
	case "selections":
		return filepath.Join(f.root, selectionsDirectory, rest)
	case "bindings":
		return filepath.Join(f.root, matchBindingDirectory, rest)
	}

	return filepath.Join(f.root, name)
}

// Read implements marketops.Files.
func (f stateFiles) Read(_ context.Context, name string) ([]byte, error) {
	raw, err := os.ReadFile(f.marketPath(name))

	if errors.Is(err, fs.ErrNotExist) {
		return nil, fs.ErrNotExist
	}

	return raw, err
}

// Write implements marketops.Files.
func (f stateFiles) Write(_ context.Context, name string, data []byte) error {
	path := f.marketPath(name)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	return writeAtomically(path, data)
}

// cutSlash splits "a/b" into ("a", "b", true).
func cutSlash(name string) (string, string, bool) {
	for index := 0; index < len(name); index++ {
		if name[index] == '/' {
			return name[:index], name[index+1:], true
		}
	}

	return "", "", false
}

// marketEngine adapts the swap engine to what the market operations need.
type marketEngine struct{ engine *swap.Engine }

// New implements marketops.Engine.
func (m marketEngine) New(ctx context.Context, req swap.NewRequest) (*swap.NewResult, error) {
	return m.engine.New(ctx, req)
}

// AcceptOffer implements marketops.Engine.
func (m marketEngine) AcceptOffer(ctx context.Context, req swap.AcceptRequest) (*swap.AcceptResult, error) {
	return m.engine.AcceptOffer(ctx, req)
}

// PublishOffer implements marketops.Engine.
func (m marketEngine) PublishOffer(ctx context.Context, swapID string) (*swap.MessageResult, error) {
	return m.engine.PublishOffer(ctx, swapID)
}

// marketEnv builds the environment the market operations run in.
//
// print receives the scan lines the reference prints while it reads; a caller that wants no
// transcript passes nil, and the worker does.
func marketEnv(engine *swap.Engine, client *nostr.Client, print func(string)) *marketops.Env {
	key, _ := engine.Identity.(*nostr.Key)

	return &marketops.Env{
		Client:   client,
		Identity: key,
		Records:  store.NewFile(stateDirectory()),
		Files:    stateFiles{root: stateDirectory()},
		Engine:   marketEngine{engine: engine},
		Random:   randReader(),
		Print:    print,
	}
}

// marketEnvOrError builds the environment, refusing a missing identity with the command's
// own wording.
func marketEnvOrError(engine *swap.Engine, client *nostr.Client, print func(string)) (*marketops.Env, error) {
	if engine == nil {
		return nil, fmt.Errorf("no swap engine")
	}

	if engine.Identity == nil {
		return nil, fmt.Errorf("no participant identity is configured")
	}

	return marketEnv(engine, client, print), nil
}
