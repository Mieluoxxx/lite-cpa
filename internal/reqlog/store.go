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
	Error         string    `json:"error"`
	ReqBody       string    `json:"req_body,omitempty"`
	RespBody      string    `json:"resp_body,omitempty"`
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
	Insert(ctx context.Context, r Record) error
	Clear(ctx context.Context) (int64, error)
	DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error)
	List(ctx context.Context, f ListFilter) ([]Record, int64, error)
	Stats(ctx context.Context, f ListFilter) (Stats, error)
	Close() error
}

// Noop is used when request logging is disabled.
type Noop struct{}

func (Noop) Insert(context.Context, Record) error                      { return nil }
func (Noop) Clear(context.Context) (int64, error)                      { return 0, nil }
func (Noop) DeleteOlderThan(context.Context, time.Time) (int64, error) { return 0, nil }
func (Noop) List(context.Context, ListFilter) ([]Record, int64, error) {
	return []Record{}, 0, nil
}
func (Noop) Stats(context.Context, ListFilter) (Stats, error) { return emptyStats(), nil }
func (Noop) Close() error                                     { return nil }
