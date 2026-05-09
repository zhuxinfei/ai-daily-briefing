package ingest

import (
	"context"
	"errors"

	"briefing-v3/internal/store"
)

// foloConfig is the JSON shape reserved for type "folo_list" Source rows.
//
// TODO Day 2: implement Folo API with cookie auth.
// See upstream wrangler.toml FOLO_DATA_API and the Folo list-export endpoint
// which needs a logged-in session cookie.
type foloConfig struct {
	ListID string `json:"list_id"`
	Cookie string `json:"cookie"`
}

// foloStubSource is a placeholder so that rows of type "folo_list" can be
// created in the database today without breaking the ingest pipeline.
// Fetch() always returns a sentinel error until Day 2 implementation lands.
type foloStubSource struct {
	row *store.Source
}

func newFoloSource(row *store.Source) (Source, error) {
	return &foloStubSource{row: row}, nil
}

func (s *foloStubSource) ID() int64    { return s.row.ID }
func (s *foloStubSource) Type() string { return s.row.Type }
func (s *foloStubSource) Name() string { return s.row.Name }

func (s *foloStubSource) Fetch(ctx context.Context) ([]*store.RawItem, error) {
	// TODO Day 2: call https://.../api/lists/{list_id}/entries with the
	// user's Folo session cookie, map entries into store.RawItem, dedup
	// via entry id.
	return nil, errors.New("folo source not yet implemented (Day 2: needs auth cookie)")
}

func init() {
	Register("folo_list", Factory(newFoloSource))
}
