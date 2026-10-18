//go:build !js

package fund

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/gjbrr/blake-swap/pkg/chain"
)

// FileStore is a Store in a directory: one JSON document per intent, named the
// way the Python engine names it.
//
// The intent is the only cover for the window between a funding transaction being
// broadcast and the swap record learning its outpoint, so the writes here are the
// ones that have to survive a crash.
type FileStore struct {
	dir string
}

// NewFileStore opens the funding-intent directory, creating nothing until a write.
func NewFileStore(dir string) *FileStore { return &FileStore{dir: dir} }

// path is where one intent lives.
func (s *FileStore) path(swapID string, ch chain.Chain) string {
	return filepath.Join(s.dir, swapID+"."+string(ch)+".json")
}

// Read implements Store.
func (s *FileStore) Read(_ context.Context, swapID string, ch chain.Chain) ([]byte, bool, error) {
	data, err := os.ReadFile(s.path(swapID, ch))

	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}

	if err != nil {
		return nil, false, err
	}

	return data, true, nil
}

// Write implements Store.
func (s *FileStore) Write(_ context.Context, swapID string, ch chain.Chain, data []byte) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}

	return writeFileAtomic(s.path(swapID, ch), data)
}

// Delete implements Store.
func (s *FileStore) Delete(_ context.Context, swapID string, ch chain.Chain) error {
	err := os.Remove(s.path(swapID, ch))

	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	return err
}

// writeFileAtomic replaces a path so a crash never leaves it truncated.
func writeFileAtomic(path string, payload []byte) error {
	temp := path + ".tmp"

	file, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)

	if err != nil {
		return err
	}

	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		_ = os.Remove(temp)

		return err
	}

	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(temp)

		return err
	}

	if err := file.Close(); err != nil {
		_ = os.Remove(temp)

		return err
	}

	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)

		return err
	}

	return nil
}
