package reqlog

import (
	"context"
)

// ListFilter selects a page of request logs for the monitor API.
type ListFilter struct {
	Limit      int
	Offset     int
	Model      string
	Upstream   string
	Protocol   string
	Status     string // "2xx", "4xx", "5xx", etc.
	ErrorsOnly bool
}

// Stats is aggregate request-log metrics for the monitor dashboard.
type Stats struct {
	Enabled       bool        `json:"enabled"`
	Total         int64       `json:"total"`
	Errors        int64       `json:"errors"`
	Success       int64       `json:"success"`
	AvgDurationMS float64     `json:"avg_duration_ms"`
	InputTokens   int64       `json:"input_tokens"`
	OutputTokens  int64       `json:"output_tokens"`
	CachedTokens  int64       `json:"cached_tokens"`
	CacheHitRate  float64     `json:"cache_hit_rate"`
	OutputTPS     float64     `json:"output_tps"`
	ByStatus      []NameCount `json:"by_status"`
	ByModel       []NameCount `json:"by_model"`
	ByUpstream    []NameCount `json:"by_upstream"`
	ByProtocol    []NameCount `json:"by_protocol"`
}

// NameCount is a named bucket used by Stats.
type NameCount struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

// ListResult is a page of logs plus the filtered total for pagination.
type ListResult struct {
	Items  []Record `json:"items"`
	Total  int64    `json:"total"`
	Limit  int      `json:"limit"`
	Offset int      `json:"offset"`
}

func normalizeListFilter(f ListFilter) ListFilter {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Limit > 200 {
		f.Limit = 200
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	return f
}

// List returns a page of records when logging is enabled.
func (l *Logger) List(ctx context.Context, f ListFilter) (ListResult, error) {
	f = normalizeListFilter(f)
	out := ListResult{Items: []Record{}, Limit: f.Limit, Offset: f.Offset}
	if l == nil || !l.Enabled() {
		return out, nil
	}
	items, total, err := l.store.List(ctx, f)
	if err != nil {
		return out, err
	}
	if items == nil {
		items = []Record{}
	}
	out.Items = items
	out.Total = total
	return out, nil
}

// Clear deletes all persisted request logs and returns the number removed.
func (l *Logger) Clear(ctx context.Context) (int64, error) {
	if l == nil || !l.Enabled() {
		return 0, nil
	}
	return l.store.Clear(ctx)
}

// Stats returns aggregate metrics for the supplied request-log filter.
func (l *Logger) Stats(ctx context.Context, f ListFilter) (Stats, error) {
	if l == nil || !l.Enabled() {
		st := emptyStats()
		st.Enabled = false
		return st, nil
	}
	st, err := l.store.Stats(ctx, f)
	if err != nil {
		return Stats{}, err
	}
	st.Enabled = true
	finalizeStats(&st)
	return st, nil
}

func emptyStats() Stats {
	return Stats{
		ByStatus:   []NameCount{},
		ByModel:    []NameCount{},
		ByUpstream: []NameCount{},
		ByProtocol: []NameCount{},
	}
}

func finalizeStats(st *Stats) {
	if st.ByStatus == nil {
		st.ByStatus = []NameCount{}
	}
	if st.ByModel == nil {
		st.ByModel = []NameCount{}
	}
	if st.ByUpstream == nil {
		st.ByUpstream = []NameCount{}
	}
	if st.ByProtocol == nil {
		st.ByProtocol = []NameCount{}
	}
	st.Success = st.Total - st.Errors
	if st.Success < 0 {
		st.Success = 0
	}
	if st.InputTokens > 0 {
		st.CacheHitRate = float64(st.CachedTokens) / float64(st.InputTokens)
	} else {
		st.CacheHitRate = 0
	}
}
