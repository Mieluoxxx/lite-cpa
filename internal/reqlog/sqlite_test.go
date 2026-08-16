package reqlog

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestSQLiteInsertAndRetention(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests.db")
	store, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	var journalMode string
	if err := store.db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if journalMode != "delete" {
		t.Fatalf("journal mode %q want delete", journalMode)
	}

	now := time.Now().UTC()
	if err := store.Insert(context.Background(), Record{
		RequestID: "r1", Timestamp: now.Add(-48 * time.Hour), Method: "POST", Path: "/v1/chat/completions",
		StatusCode: 200, Model: "m", Protocol: "chat", DurationMS: 12,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Insert(context.Background(), Record{
		RequestID: "r2", Timestamp: now, Method: "POST", Path: "/v1/messages",
		StatusCode: 200, Model: "c", Protocol: "claude", DurationMS: 5,
	}); err != nil {
		t.Fatal(err)
	}
	n, err := store.DeleteOlderThan(context.Background(), now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("deleted %d want 1", n)
	}
}

func TestSQLiteRetentionNanoOrdering(t *testing.T) {
	// Ensure fractional-second boundaries compare correctly (INTEGER ns, not TEXT).
	dir := t.TempDir()
	store, err := OpenSQLite(filepath.Join(dir, "requests.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	base := time.Date(2026, 7, 21, 12, 0, 0, 100_000_000, time.UTC)  // .1s
	later := time.Date(2026, 7, 21, 12, 0, 0, 120_000_000, time.UTC) // .12s
	// Lexical RFC3339Nano would put ".12" before ".1"; integer ns must not.
	if err := store.Insert(context.Background(), Record{
		RequestID: "old", Timestamp: base, Method: "POST", Path: "/a", StatusCode: 200,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Insert(context.Background(), Record{
		RequestID: "new", Timestamp: later, Method: "POST", Path: "/b", StatusCode: 200,
	}); err != nil {
		t.Fatal(err)
	}
	// cutoff between base and later
	n, err := store.DeleteOlderThan(context.Background(), base.Add(10*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("deleted %d want 1 (only .1s row)", n)
	}
}

func TestSQLiteMigratesLegacyTextTS(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")

	// Create a legacy DB with TEXT RFC3339Nano timestamps.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
CREATE TABLE request_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  request_id TEXT NOT NULL DEFAULT '',
  ts TEXT NOT NULL,
  method TEXT NOT NULL DEFAULT '',
  path TEXT NOT NULL DEFAULT '',
  status_code INTEGER NOT NULL DEFAULT 0,
  model TEXT NOT NULL DEFAULT '',
  protocol TEXT NOT NULL DEFAULT '',
  provider TEXT NOT NULL DEFAULT '',
  upstream TEXT NOT NULL DEFAULT '',
  user_agent TEXT NOT NULL DEFAULT '',
  duration_ms INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT '',
  req_body TEXT NOT NULL DEFAULT '',
  resp_body TEXT NOT NULL DEFAULT ''
);`); err != nil {
		t.Fatal(err)
	}
	// Lexical trap: ".12Z" < ".1Z" as text, but .12s is newer.
	oldTS := "2026-07-21T12:00:00.1Z"
	newTS := "2026-07-21T12:00:00.12Z"
	if _, err := db.Exec(`INSERT INTO request_logs (request_id, ts, method, path, status_code) VALUES
		('old', ?, 'POST', '/old', 200),
		('new', ?, 'POST', '/new', 200)`, oldTS, newTS); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	// Open through production path — should migrate TEXT → INTEGER ns.
	store, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Cutoff between .1s and .12s.
	cutoff := time.Date(2026, 7, 21, 12, 0, 0, 110_000_000, time.UTC)
	n, err := store.DeleteOlderThan(context.Background(), cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("after migration deleted %d want 1 (only old .1s row)", n)
	}

	// Confirm remaining row is the newer one.
	var pathLeft string
	if err := store.db.QueryRow(`SELECT path FROM request_logs`).Scan(&pathLeft); err != nil {
		t.Fatal(err)
	}
	if pathLeft != "/new" {
		t.Fatalf("remaining path %q want /new", pathLeft)
	}
}

func TestSQLiteListAndStats(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLite(filepath.Join(dir, "requests.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := time.Now().UTC()
	rows := []Record{
		{RequestID: "a", Timestamp: now.Add(-2 * time.Second), Method: "POST", Path: "/v1/messages", StatusCode: 200, Model: "claude-x", Protocol: "claude", Upstream: "anth", DurationMS: 10, InputTokens: 100, OutputTokens: 10, CachedTokens: 40, UsageComplete: true},
		{RequestID: "b", Timestamp: now.Add(-1 * time.Second), Method: "POST", Path: "/v1/responses", StatusCode: 429, Model: "gpt-x", Protocol: "responses", Upstream: "laysath", DurationMS: 20, InputTokens: 200, OutputTokens: 20, CachedTokens: 50, UsageComplete: true, Error: "rate"},
		{RequestID: "c", Timestamp: now, Method: "POST", Path: "/v1/chat/completions", StatusCode: 500, Model: "gpt-x", Protocol: "chat", Upstream: "laysath", DurationMS: 30, Error: "boom"},
		{RequestID: "d", Timestamp: now.Add(-3 * time.Second), Method: "POST", Path: "/v1/responses", StatusCode: 200, Outcome: OutcomeClientCanceled, Model: "gpt-canceled", Protocol: "responses", Upstream: "openai", DurationMS: 40, Error: "context canceled"},
	}
	for _, r := range rows {
		if err := store.Insert(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}

	items, total, err := store.List(context.Background(), ListFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 {
		t.Fatalf("total %d want 4", total)
	}
	if len(items) != 2 {
		t.Fatalf("page len %d want 2", len(items))
	}
	if items[0].RequestID != "c" || items[1].RequestID != "b" {
		t.Fatalf("order got %q %q", items[0].RequestID, items[1].RequestID)
	}
	if items[1].InputTokens != 200 || items[1].OutputTokens != 20 || items[1].CachedTokens != 50 || !items[1].UsageComplete || items[1].Outcome != OutcomeError {
		t.Fatalf("listed tokens = (%d, %d, %d), want (200, 20, 50)", items[1].InputTokens, items[1].OutputTokens, items[1].CachedTokens)
	}

	errItems, errTotal, err := store.List(context.Background(), ListFilter{ErrorsOnly: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if errTotal != 2 || len(errItems) != 2 {
		t.Fatalf("errors total=%d len=%d", errTotal, len(errItems))
	}
	canceledItems, canceledTotal, err := store.List(context.Background(), ListFilter{Outcome: OutcomeClientCanceled, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if canceledTotal != 1 || len(canceledItems) != 1 || canceledItems[0].RequestID != "d" {
		t.Fatalf("canceled total=%d items=%#v", canceledTotal, canceledItems)
	}

	modelItems, modelTotal, err := store.List(context.Background(), ListFilter{Model: "gpt-x", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if modelTotal != 2 || len(modelItems) != 2 {
		t.Fatalf("model filter total=%d len=%d", modelTotal, len(modelItems))
	}

	st, err := store.Stats(context.Background(), ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 4 || st.Errors != 2 || st.Success != 1 || st.Canceled != 1 || st.Unknown != 0 {
		t.Fatalf("stats total=%d errors=%d success=%d canceled=%d unknown=%d", st.Total, st.Errors, st.Success, st.Canceled, st.Unknown)
	}
	if st.UsageIncomplete != 2 {
		t.Fatalf("usage incomplete=%d want 2", st.UsageIncomplete)
	}
	if st.AvgDurationMS < 24 || st.AvgDurationMS > 26 {
		t.Fatalf("avg duration %v", st.AvgDurationMS)
	}
	if st.InputTokens != 300 || st.OutputTokens != 30 || st.CachedTokens != 90 || st.OutputTPS != 1000 {
		t.Fatalf("token stats input=%d output=%d cached=%d tps=%v", st.InputTokens, st.OutputTokens, st.CachedTokens, st.OutputTPS)
	}
	if st.CacheHitRate != 0.3 {
		t.Fatalf("cache hit rate %v want 0.3", st.CacheHitRate)
	}
	if len(st.ByUpstream) == 0 || st.ByUpstream[0].Name != "laysath" || st.ByUpstream[0].Count != 2 {
		t.Fatalf("by_upstream %#v", st.ByUpstream)
	}

	filteredStats, err := store.Stats(context.Background(), ListFilter{Model: "gpt-x", Upstream: "laysath"})
	if err != nil {
		t.Fatal(err)
	}
	if filteredStats.Total != 2 || filteredStats.Errors != 2 || filteredStats.Success != 0 {
		t.Fatalf("filtered stats total=%d errors=%d success=%d", filteredStats.Total, filteredStats.Errors, filteredStats.Success)
	}
	if filteredStats.InputTokens != 200 || filteredStats.OutputTokens != 20 || filteredStats.CachedTokens != 50 {
		t.Fatalf("filtered token stats input=%d output=%d cached=%d", filteredStats.InputTokens, filteredStats.OutputTokens, filteredStats.CachedTokens)
	}
	if len(filteredStats.ByModel) != 1 || filteredStats.ByModel[0] != (NameCount{Name: "gpt-x", Count: 2}) {
		t.Fatalf("filtered by_model %#v", filteredStats.ByModel)
	}
	if len(filteredStats.ByUpstream) != 1 || filteredStats.ByUpstream[0] != (NameCount{Name: "laysath", Count: 2}) {
		t.Fatalf("filtered by_upstream %#v", filteredStats.ByUpstream)
	}

	deleted, err := store.Clear(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 4 {
		t.Fatalf("cleared %d want 4", deleted)
	}
	clearedItems, clearedTotal, err := store.List(context.Background(), ListFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if clearedTotal != 0 || len(clearedItems) != 0 {
		t.Fatalf("after clear total=%d len=%d", clearedTotal, len(clearedItems))
	}
}
