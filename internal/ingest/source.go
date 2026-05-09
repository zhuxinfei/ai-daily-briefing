// Package ingest defines the data source abstraction and concrete implementations.
//
// Each source type has its own file in this package:
//   - github_trending.go
//   - smolai_rss.go
//   - rss.go (generic RSS)
//   - folo.go (Day 2+)
//
// The Registry builds Source instances from store.Source rows based on
// the Type field.
package ingest

import (
	"context"

	"briefing-v3/internal/store"
)

// Source is an adapter that fetches raw items from one external provider.
// Implementations MUST:
//   - Not mutate the database (caller persists).
//   - Be context-aware and respect cancellation.
//   - Use sensible HTTP timeouts (e.g. 10s).
//   - Return store.RawItem values with ExternalID populated for dedup.
type Source interface {
	// ID returns the database row id for this source.
	ID() int64

	// Type returns the source type tag, e.g. "github_trending".
	Type() string

	// Name returns the display name.
	Name() string

	// Fetch pulls latest items from the upstream provider.
	Fetch(ctx context.Context) ([]*store.RawItem, error)
}

// Factory constructs a Source from a store.Source row.
// Concrete factories live alongside the source implementations and
// register themselves in the package-level registry.
type Factory func(row *store.Source) (Source, error)

// registry maps source type strings to factories.
var registry = map[string]Factory{}

// Register makes a source type available to the runtime.
// Call this from each source implementation's init().
func Register(typeName string, factory Factory) {
	registry[typeName] = factory
}

// Build returns a ready-to-fetch Source for the given database row.
func Build(row *store.Source) (Source, error) {
	factory, ok := registry[row.Type]
	if !ok {
		return nil, ErrUnknownSourceType{Type: row.Type}
	}
	return factory(row)
}

// ErrUnknownSourceType is returned when a source row's Type is not registered.
type ErrUnknownSourceType struct {
	Type string
}

func (e ErrUnknownSourceType) Error() string {
	return "ingest: unknown source type: " + e.Type
}
