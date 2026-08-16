package reqlog_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/Mieluoxxx/lite-cpa/internal/reqlog"
	_ "modernc.org/sqlite"
)

func TestOpenMigratesRequestLogColumns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE request_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id TEXT NOT NULL DEFAULT '',
    ts INTEGER NOT NULL,
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
    resp_body TEXT NOT NULL DEFAULT '',
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0
  );`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO request_logs
    (request_id, ts, method, path, status_code, error, input_tokens, output_tokens)
    VALUES
    ('completed', 1, 'POST', '/v1/responses', 200, '', 12, 3),
    ('canceled', 2, 'POST', '/v1/responses', 200, 'context canceled', 0, 0),
    ('failed', 3, 'POST', '/v1/responses', 500, 'upstream failed', 0, 0),
    ('ambiguous', 4, 'POST', '/v1/responses', 200, '', 0, 0)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := reqlog.OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	db2, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	rows, err := db2.Query(`PRAGMA table_info(request_logs)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		cols[name] = true
	}
	for _, column := range []string{"cached_tokens", "outcome", "usage_complete"} {
		if !cols[column] {
			t.Fatalf("missing %s: %#v", column, cols)
		}
	}
	logs, total, err := store.List(context.Background(), reqlog.ListFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 {
		t.Fatalf("migrated total=%d want 4", total)
	}
	byID := make(map[string]reqlog.Record, len(logs))
	for _, record := range logs {
		byID[record.RequestID] = record
	}
	if got := byID["completed"]; got.Outcome != reqlog.OutcomeCompleted || !got.UsageComplete {
		t.Fatalf("completed migration = %#v", got)
	}
	if got := byID["canceled"]; got.Outcome != reqlog.OutcomeClientCanceled || got.UsageComplete {
		t.Fatalf("canceled migration = %#v", got)
	}
	if got := byID["failed"]; got.Outcome != reqlog.OutcomeError || got.UsageComplete {
		t.Fatalf("failed migration = %#v", got)
	}
	if got := byID["ambiguous"]; got.Outcome != reqlog.OutcomeUnknown || got.UsageComplete {
		t.Fatalf("ambiguous migration = %#v", got)
	}
	st, err := store.Stats(context.Background(), reqlog.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 4 || st.Success != 1 || st.Errors != 1 || st.Canceled != 1 || st.Unknown != 1 || st.UsageIncomplete != 3 {
		t.Fatalf("migrated stats = %#v", st)
	}
}
