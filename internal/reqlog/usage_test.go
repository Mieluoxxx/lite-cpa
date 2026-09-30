package reqlog

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func openTestSQLite(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := OpenSQLite(filepath.Join(t.TempDir(), "requests.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestSQLiteInsertMaintainsUsageHourly(t *testing.T) {
	store := openTestSQLite(t)
	ctx := context.Background()
	hour := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)

	for _, r := range []Record{
		// One request that failed once, then succeeded on another key.
		{RequestID: "a", Timestamp: hour.Add(time.Minute), UserAgent: "pi (darwin)", Model: "gpt-5", StatusCode: 502, Outcome: OutcomeError, Retried: true},
		{RequestID: "a", Timestamp: hour.Add(2 * time.Minute), UserAgent: "pi (darwin)", Model: "gpt-5", StatusCode: 200, Outcome: OutcomeCompleted,
			InputTokens: 100, OutputTokens: 10, CachedTokens: 40, UsageComplete: true},
		// A request whose only attempt failed.
		{RequestID: "b", Timestamp: hour.Add(3 * time.Minute), UserAgent: "pi (darwin)", Model: "gpt-5", StatusCode: 500, Outcome: OutcomeError},
		// Same agent in the next hour.
		{RequestID: "c", Timestamp: hour.Add(61 * time.Minute), UserAgent: "pi (darwin)", Model: "gpt-5", StatusCode: 200, Outcome: OutcomeCompleted,
			InputTokens: 5, OutputTokens: 1, UsageComplete: true},
	} {
		if err := store.Insert(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	buckets, err := store.Usage(ctx, hour, hour.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 2 {
		t.Fatalf("buckets=%d want 2: %+v", len(buckets), buckets)
	}
	first := buckets[0]
	if first.Hour != hour.Unix() || first.Attempts != 3 || first.Requests != 2 || first.Success != 1 || first.Errors != 1 {
		t.Fatalf("first bucket counts wrong: %+v", first)
	}
	if first.Incomplete != 1 || first.InputTokens != 100 || first.OutputTokens != 10 || first.CachedTokens != 40 {
		t.Fatalf("first bucket usage wrong: %+v", first)
	}
	if first.LastSeenMS != hour.Add(3*time.Minute).UnixMilli() {
		t.Fatalf("last_seen_ms=%d want %d", first.LastSeenMS, hour.Add(3*time.Minute).UnixMilli())
	}
	if buckets[1].Hour != hour.Add(time.Hour).Unix() || buckets[1].Requests != 1 {
		t.Fatalf("second bucket wrong: %+v", buckets[1])
	}

	// Clearing raw logs must leave the rollup intact.
	if _, err := store.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	buckets, err = store.Usage(ctx, hour, hour.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 2 {
		t.Fatalf("rollup lost after clear: %+v", buckets)
	}
}

func TestSQLiteMigrationBackfillsRetriedAndUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	hour := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`CREATE TABLE request_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  request_id TEXT NOT NULL DEFAULT '',
  ts INTEGER NOT NULL,
  method TEXT NOT NULL DEFAULT '',
  path TEXT NOT NULL DEFAULT '',
  status_code INTEGER NOT NULL DEFAULT 0,
  outcome TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  protocol TEXT NOT NULL DEFAULT '',
  provider TEXT NOT NULL DEFAULT '',
  upstream TEXT NOT NULL DEFAULT '',
  user_agent TEXT NOT NULL DEFAULT '',
  duration_ms INTEGER NOT NULL DEFAULT 0,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  cached_tokens INTEGER NOT NULL DEFAULT 0,
  usage_complete INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT '',
  req_body TEXT NOT NULL DEFAULT '',
  resp_body TEXT NOT NULL DEFAULT ''
)`); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		requestID string
		offset    time.Duration
		status    int
		outcome   string
		input     int64
		complete  int
	}{
		{"a", time.Minute, 502, OutcomeError, 0, 0},
		{"a", 2 * time.Minute, 502, OutcomeError, 0, 0},
		{"a", 3 * time.Minute, 200, OutcomeCompleted, 70, 1},
		{"b", 4 * time.Minute, 200, OutcomeCompleted, 30, 1},
	} {
		if _, err := db.Exec(`INSERT INTO request_logs (request_id, ts, status_code, outcome, model, user_agent, input_tokens, usage_complete)
VALUES (?, ?, ?, ?, 'gpt-5', 'codex_cli_rs/0.1', ?, ?)`,
			row.requestID, hour.Add(row.offset).UnixNano(), row.status, row.outcome, row.input, row.complete); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()

	store, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	var retried int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM request_logs WHERE retried = 1`).Scan(&retried); err != nil {
		t.Fatal(err)
	}
	if retried != 2 {
		t.Fatalf("retried rows=%d want 2", retried)
	}
	buckets, err := store.Usage(context.Background(), hour, hour.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 1 {
		t.Fatalf("buckets=%+v want 1", buckets)
	}
	b := buckets[0]
	if b.Attempts != 4 || b.Requests != 2 || b.Success != 2 || b.Errors != 0 || b.InputTokens != 100 {
		t.Fatalf("backfilled bucket wrong: %+v", b)
	}

	// Reopening must not backfill twice.
	_ = store.Close()
	store, err = OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	buckets, err = store.Usage(context.Background(), hour, hour.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 1 || buckets[0].Attempts != 4 {
		t.Fatalf("rollup changed on reopen: %+v", buckets)
	}
}

func TestLoggerCleanupKeepsRowsAndStripsOldBodies(t *testing.T) {
	store := openTestSQLite(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, r := range []Record{
		{RequestID: "old", Timestamp: now.Add(-10 * 24 * time.Hour), ReqBody: "req", RespBody: "resp"},
		{RequestID: "new", Timestamp: now.Add(-time.Hour), ReqBody: "req", RespBody: "resp"},
	} {
		if err := store.Insert(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	l := &Logger{store: store, bodyRetention: 7 * 24 * time.Hour}
	l.cleanup(ctx)

	logs, total, err := store.List(ctx, ListFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Fatalf("rows=%d want 2 (retention 0 keeps rows forever)", total)
	}
	for _, r := range logs {
		stripped := r.ReqBody == "" && r.RespBody == ""
		if (r.RequestID == "old") != stripped {
			t.Fatalf("row %s stripped=%v", r.RequestID, stripped)
		}
	}
	if l.bodiesClearedTo.IsZero() {
		t.Fatal("body cleanup watermark not advanced")
	}

	// Later passes only scan the window since the previous cutoff.
	n, err := store.StripBodies(ctx, now.Add(-2*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("windowed strip cleared %d rows want 1", n)
	}
}

func TestSQLiteListFiltersByTimeAndUserAgent(t *testing.T) {
	store := openTestSQLite(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	for i, ua := range []string{"pi (darwin)", "codex_cli_rs/0.1", "pi (darwin)", ""} {
		if err := store.Insert(ctx, Record{RequestID: string(rune('a' + i)), Timestamp: base.Add(time.Duration(i) * time.Hour), UserAgent: ua}); err != nil {
			t.Fatal(err)
		}
	}

	_, total, err := store.List(ctx, ListFilter{Limit: 10, UserAgents: []string{"pi (darwin)", ""}})
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Fatalf("ua filter total=%d want 3", total)
	}
	_, total, err = store.List(ctx, ListFilter{Limit: 10, From: base.Add(time.Hour), To: base.Add(3 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Fatalf("time filter total=%d want 2", total)
	}
	_, total, err = store.List(ctx, ListFilter{Limit: 10, UserAgents: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Fatalf("empty ua list total=%d want 0", total)
	}
}

func TestAggregateUsageInLocalTime(t *testing.T) {
	// 2026-09-30 20:00 UTC is Thursday 2026-10-01 04:00 in UTC+8.
	h1 := time.Date(2026, 9, 30, 20, 0, 0, 0, time.UTC).Unix()
	// 2026-09-30 15:00 UTC is still Wednesday 23:00 in UTC+8.
	h2 := time.Date(2026, 9, 30, 15, 0, 0, 0, time.UTC).Unix()
	buckets := []UsageBucket{
		{Hour: h1, UserAgent: "pi", Model: "gpt-5", Requests: 2, InputTokens: 10, LastSeenMS: 5},
		{Hour: h1, UserAgent: "pi", Model: "grok", Requests: 1},
		{Hour: h2, UserAgent: "pi", Model: "gpt-5", Requests: 3, InputTokens: 20, LastSeenMS: 9},
	}
	offset := int64(8 * 3600)

	daily := AggregateDaily(buckets, offset)
	if len(daily) != 3 || daily[0].Day != "2026-09-30" || daily[1].Day != "2026-10-01" || daily[1].Model != "gpt-5" {
		t.Fatalf("daily=%+v", daily)
	}

	weekly := AggregateWeekHour(buckets, offset)
	if len(weekly) != 2 {
		t.Fatalf("weekhour=%+v", weekly)
	}
	if weekly[0].Weekday != 2 || weekly[0].HourOfDay != 23 || weekly[0].Requests != 3 {
		t.Fatalf("wednesday slot=%+v", weekly[0])
	}
	if weekly[1].Weekday != 3 || weekly[1].HourOfDay != 4 || weekly[1].Requests != 3 || weekly[1].LastSeenMS != 5 {
		t.Fatalf("thursday slot=%+v", weekly[1])
	}

	from, first, last := UsageWindow(time.Date(2026, 9, 30, 20, 30, 0, 0, time.UTC), 7, offset)
	if first != "2026-09-25" || last != "2026-10-01" || !from.Equal(time.Date(2026, 9, 24, 16, 0, 0, 0, time.UTC)) {
		t.Fatalf("window from=%s first=%s last=%s", from, first, last)
	}
}
