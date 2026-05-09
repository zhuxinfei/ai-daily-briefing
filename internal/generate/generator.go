// Package generate is responsible for LLM-driven content creation:
// the "industry insight" and "takeaways for us" sections of each issue.
//
// The concrete implementation (openai.go) should:
//   - Use an OpenAI-compatible API with configurable base URL and model.
//   - Enforce a banned-pattern set to reject operational/hype language.
//   - Retry up to 3 times with a repair prompt if validation fails.
//   - Fall back to an insight-less delivery if the LLM is unavailable,
//     rather than blocking the whole pipeline.
package generate

import (
	"context"

	"briefing-v3/internal/store"
)

// Input bundles everything the Generator needs to produce an insight.
type Input struct {
	Issue    *store.Issue
	Items    []*store.IssueItem // sorted by section, seq
	RawItems []*store.RawItem   // original fetched items, useful for source-level evidence
}

// Generator produces insights for an Issue using an LLM.
type Generator interface {
	// GenerateInsight returns the industry insight and takeaways for the
	// given issue. The returned IssueInsight is NOT persisted; callers
	// should pass it to Store.UpsertIssueInsight.
	//
	// Implementations should:
	//   - Populate Model, Temperature, RetryCount, GeneratedAt.
	//   - Return a non-nil IssueInsight even on partial failure (with
	//     empty fields) so callers can still publish a degraded issue.
	//   - Respect ctx cancellation and apply per-request timeouts.
	GenerateInsight(ctx context.Context, in *Input) (*store.IssueInsight, error)
}
