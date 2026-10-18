package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/nostr"
)

// dirCache is a relay event cache in a directory, one file per event id.
//
// The market read needs it for a reason that is not performance: a relay read is a
// discovery window, so a cancellation seen today can be invisible tomorrow while the
// offer it cancelled is still inside the window. The reference keeps authentic
// evidence locally for exactly this, and merges it back, re-verified, before deriving
// state.
type dirCache struct {
	dir string
}

// Put writes one event.
func (c dirCache) Put(_ context.Context, e event.Event) error {
	if e.ID == "" {
		return fmt.Errorf("cannot cache an event with no id")
	}

	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return err
	}

	encoded, err := json.Marshal(e)

	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(c.dir, e.ID+".json"), encoded, 0o600)
}

// Get reads one event back.
func (c dirCache) Get(_ context.Context, id string) (event.Event, bool, error) {
	raw, err := os.ReadFile(filepath.Join(c.dir, id+".json"))

	if os.IsNotExist(err) {
		return event.Event{}, false, nil
	}

	if err != nil {
		return event.Event{}, false, err
	}

	var cached event.Event

	if err := json.Unmarshal(raw, &cached); err != nil {
		// A corrupt file is not evidence; forgetting it is better than failing the
		// whole read because one cached event is unreadable.
		return event.Event{}, false, nil
	}

	return cached, true, nil
}

// IDs lists the cached event ids.
func (c dirCache) IDs(_ context.Context) ([]string, error) {
	entries, err := os.ReadDir(c.dir)

	if os.IsNotExist(err) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(entries))

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		ids = append(ids, strings.TrimSuffix(entry.Name(), ".json"))
	}

	sort.Strings(ids)

	return ids, nil
}

// cachedEvents loads every cached event, ignoring the ones that no longer read.
func cachedEvents(ctx context.Context, cache nostr.Cache) []event.Event {
	ids, err := cache.IDs(ctx)

	if err != nil {
		return nil
	}

	events := make([]event.Event, 0, len(ids))

	for _, id := range ids {
		cached, ok, err := cache.Get(ctx, id)

		if err != nil || !ok {
			continue
		}

		events = append(events, cached)
	}

	return events
}
