//go:build !js

package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gjbrr/blake-swap/pkg/model"
)

// File is a Store in a directory, laid out the way the Python engine lays it out.
type File struct {
	base string
}

// NewFile opens the state tree under base, creating nothing until a write.
func NewFile(base string) *File { return &File{base: base} }

// Base returns the directory the store was opened on.
func (f *File) Base() string { return f.base }

// recordPath is where one swap record lives.
func (f *File) recordPath(swapID string) string {
	return filepath.Join(f.base, swapID+".json")
}

// archivePath is where one archived record lives.
func (f *File) archivePath(swapID string) string {
	return filepath.Join(f.base, "archive", swapID+".json")
}

// messagePath is where one transport message lives.
func (f *File) messagePath(swapID, kind string) string {
	return filepath.Join(f.base, "messages", swapID+"."+kind+".json")
}

// Save implements Store.
func (f *File) Save(_ context.Context, role string, swap *model.Swap) error {
	if err := ValidateRole(role); err != nil {
		return err
	}

	if swap == nil {
		return fmt.Errorf("swap is nil")
	}

	if err := swap.Validate(); err != nil {
		return err
	}

	encoded, err := encodeRecord(swap)

	if err != nil {
		return err
	}

	return writeAtomic(f.recordPath(swap.SwapID), encoded, true)
}

// Load implements Store.
func (f *File) Load(_ context.Context, swapID string) (*model.Swap, error) {
	if err := ValidateSwapID(swapID); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(f.recordPath(swapID))

	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s record %s: %w", swapID, swapID, ErrNotFound)
	}

	if err != nil {
		return nil, err
	}

	return decodeSwap(data, nil)
}

// List implements Store.
func (f *File) List(context.Context) ([]string, error) {
	entries, err := os.ReadDir(f.base)

	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	var ids []string

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		if id, ok := CanonicalID(entry.Name()); ok {
			ids = append(ids, id)
		}
	}

	sort.Strings(ids)

	return ids, nil
}

// Retired implements Store.
func (f *File) Retired(_ context.Context, swapID string) (bool, error) {
	_, err := os.Stat(f.archivePath(swapID))

	if err == nil {
		return true, nil
	}

	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}

	return false, err
}

// Archive implements Store.
func (f *File) Archive(_ context.Context, role, swapID string) error {
	if err := ValidateRole(role); err != nil {
		return err
	}

	source := f.recordPath(swapID)
	target := f.archivePath(swapID)

	if _, err := os.Stat(source); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s: %w", swapID, ErrNotFound)
		}

		return err
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}

	// Refuse to overwrite an archived record: it may be the only copy of a swap
	// that was already retired, and replacing it would destroy the evidence.
	if _, err := os.Stat(target); err == nil {
		return fmt.Errorf("an archived record for %s already exists", swapID)
	}

	if err := os.Rename(source, target); err != nil {
		return err
	}

	return syncDir(filepath.Dir(target))
}

// SendMessage implements Store.
func (f *File) SendMessage(
	_ context.Context, swapID, kind, sender string, body map[string]any,
) error {
	if err := ValidateSwapID(swapID); err != nil {
		return err
	}

	if err := ValidateSender(sender); err != nil {
		return err
	}

	if body == nil {
		return fmt.Errorf("a transport message must have a body")
	}

	encoded, err := encodeJSON(fileEnvelope{
		Version:      2,
		SenderPubkey: sender,
		Message:      body,
	}, true)

	if err != nil {
		return err
	}

	// A transport spool rather than protocol state: the message can be re-sent,
	// so the rename does not need to be durable. It does need to be atomic, so
	// the peer never reads half an envelope.
	return writeAtomic(f.messagePath(swapID, kind), encoded, false)
}

// ReceiveMessage implements Store.
func (f *File) ReceiveMessage(_ context.Context, swapID, kind string) (*Message, error) {
	if err := ValidateSwapID(swapID); err != nil {
		return nil, err
	}

	path := f.messagePath(swapID, kind)

	data, err := os.ReadFile(path)

	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("No %s message: %s: %w", kind, swapID, ErrNotFound)
	}

	if err != nil {
		return nil, err
	}

	message, err := decodeMessage(data, kind)

	if err != nil {
		return nil, err
	}

	message.Path = path

	return message, nil
}

// ListMessages implements Store.
func (f *File) ListMessages(_ context.Context, swapID string) ([]string, error) {
	if err := ValidateSwapID(swapID); err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(filepath.Join(f.base, "messages"))

	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	prefix := swapID + "."

	var kinds []string

	for _, entry := range entries {
		name := entry.Name()

		if entry.IsDir() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".json") {
			continue
		}

		kinds = append(kinds, strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".json"))
	}

	sort.Strings(kinds)

	return kinds, nil
}

// writeAtomic replaces a path with payload so a crash never leaves it truncated.
//
// The bytes go to a sibling temporary file which is flushed and fsynced before an
// atomic rename publishes it. A reader therefore sees either the complete
// previous contents or the complete new contents, never a prefix of either.
//
// `durable` additionally persists the rename: without it a power failure can
// lose the directory entry even though the contents reached the disk, so
// protocol state asks for it and the message spool does not.
func writeAtomic(path string, payload []byte, durable bool) error {
	directory := filepath.Dir(path)

	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}

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

	if !durable {
		return nil
	}

	return syncDir(directory)
}

// syncDir persists a directory entry update where the platform allows it.
//
// Some filesystems do not support fsync on a directory, and a failure there says
// nothing about whether the data write succeeded, so it is deliberately not
// fatal.
func syncDir(directory string) error {
	handle, err := os.Open(directory)

	if err != nil {
		return nil
	}

	_ = handle.Sync()
	_ = handle.Close()

	return nil
}
