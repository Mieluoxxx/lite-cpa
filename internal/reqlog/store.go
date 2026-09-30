package reqlog

import (
	"context"
	"time"
)

const (
	OutcomeCompleted      = "completed"
	OutcomeError          = "error"
	OutcomeClientCanceled = "client_canceled"
	OutcomeUnknown        = "unknown"
)

// Record is a single API request metadata row.
// Bodies are optional and only stored when store-body is enabled.
type Record struct {
	ID            int64     `json:"id"`
	RequestID     string    `json:"request_id"`
	Timestamp     time.Time `json:"timestamp"`
	Method        string    `json:"method"`
	Path          string    `json:"path"`
	StatusCode    int       `json:"status_code"`
	Outcome       string    `json:"outcome"`
	Model         string    `json:"model"`
	Protocol      string    `json:"protocol"` // chat | responses | claude
	Provider      string    `json:"provider"` // upstream provider type
	Upstream      string    `json:"upstream"` // provider profile name
	UserAgent     string    `json:"user_agent"`
	DurationMS    int64     `json:"duration_ms"`
	InputTokens   int64     `json:"input_tokens"`
	OutputTokens  int64     `json:"output_tokens"`
	CachedTokens  int64     `json:"cached_tokens"`
	UsageComplete bool      `json:"usage_complete"`
	// Retried marks a failed attempt that a later row of the same request
	// superseded; usage stats count only the final row of each request.
	Retried  bool   `json:"retried"`
	Error    string `json:"error"`
	ReqBody  string `json:"req_body,omitempty"`
	RespBody string `json:"resp_body,omitempty"`
}

func normalizeRecord(r Record) Record {
	if r.Outcome != "" {
		return r
	}
	if r.StatusCode >= 400 || r.Error != "" {
		r.Outcome = OutcomeError
	} else {
		r.Outcome = OutcomeCompleted
	}
	return r
}

// Store persists request records and supports retention cleanup and monitor queries.
type Store interface {
	// Insert writes the row and folds it into usage_hourly in one transaction.
	Insert(ctx context.Context, r Record) error
	// Clear deletes raw rows only; the hourly usage rollup is kept.
	Clear(ctx context.Context) (int64, error)
	DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error)
	// StripBodies clears bodies of rows with after <= ts < before; a zero
	// after means no lower bound.
	StripBodies(ctx context.Context, after, before time.Time) (int64, error)
	List(ctx context.Context, f ListFilter) ([]Record, int64, error)
	Stats(ctx context.Context, f ListFilter) (Stats, error)
	// Usage returns hourly rollup buckets with from <= hour < to.
	Usage(ctx context.Context, from, to time.Time) ([]UsageBucket, error)
	Close() error
}

// Noop is used when request logging is disabled.
type Noop struct{}

func (Noop) Insert(context.Context, Record) error                      { return nil }
func (Noop) Clear(context.Context) (int64, error)                      { return 0, nil }
func (Noop) DeleteOlderThan(context.Context, time.Time) (int64, error) { return 0, nil }
func (Noop) StripBodies(context.Context, time.Time, time.Time) (int64, error) {
	return 0, nil
}
func (Noop) List(context.Context, ListFilter) ([]Record, int64, error) {
	return []Record{}, 0, nil
}
func (Noop) Stats(context.Context, ListFilter) (Stats, error) { return emptyStats(), nil }
func (Noop) Usage(context.Context, time.Time, time.Time) ([]UsageBucket, error) {
	return []UsageBucket{}, nil
}
func (Noop) Close() error { return nil }
