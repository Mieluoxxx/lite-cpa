package reqlog

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type SQLiteStore struct {
	db *sql.DB
}

func OpenSQLite(path string) (*SQLiteStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	// Rollback journaling avoids WAL's shared-memory index on Docker/macOS bind mounts.
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(DELETE)&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &SQLiteStore{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLiteStore) migrate() error {
	// Create the current schema if the table is missing.
	if _, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS request_logs (
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
  retried INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT '',
  req_body TEXT NOT NULL DEFAULT '',
  resp_body TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_request_logs_ts ON request_logs(ts);
`); err != nil {
		return err
	}
	if err := s.migrateTsToIntegerIfNeeded(); err != nil {
		return err
	}
	added, err := s.addColumns()
	if err != nil {
		return err
	}
	if err := s.backfillOutcome(); err != nil {
		return err
	}
	if added["retried"] {
		if err := s.backfillRetried(); err != nil {
			return fmt.Errorf("backfill retried: %w", err)
		}
	}
	return s.ensureUsageHourly()
}

// addColumns upgrades older schemas and reports which columns it added.
func (s *SQLiteStore) addColumns() (map[string]bool, error) {
	added := make(map[string]bool)
	for _, column := range []string{
		"input_tokens INTEGER NOT NULL DEFAULT 0",
		"output_tokens INTEGER NOT NULL DEFAULT 0",
		"cached_tokens INTEGER NOT NULL DEFAULT 0",
		"outcome TEXT NOT NULL DEFAULT ''",
		"usage_complete INTEGER NOT NULL DEFAULT 0",
		"retried INTEGER NOT NULL DEFAULT 0",
	} {
		name := strings.Fields(column)[0]
		if _, err := s.db.Exec(`ALTER TABLE request_logs ADD COLUMN ` + column); err != nil {
			if !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
				return nil, fmt.Errorf("add %s: %w", name, err)
			}
			continue
		}
		added[name] = true
	}
	return added, nil
}

// backfillRetried infers the flag for rows written before it existed: every
// row except the last one of a request_id was superseded by a later attempt.
func (s *SQLiteStore) backfillRetried() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		// A temp table plus index keeps this O(n log n); a correlated subquery
		// on the unindexed request_id would be quadratic on large tables.
		`CREATE TEMP TABLE retried_last AS
SELECT request_id, MAX(id) AS last_id FROM request_logs
WHERE request_id <> '' GROUP BY request_id HAVING COUNT(*) > 1`,
		`CREATE INDEX temp.idx_retried_last ON retried_last(request_id)`,
		`UPDATE request_logs SET retried = 1
WHERE id < (SELECT last_id FROM retried_last WHERE retried_last.request_id = request_logs.request_id)`,
		`DROP TABLE retried_last`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ensureUsageHourly creates the rollup table and, when it is new, fills it
// from existing rows in the same transaction so a crash just retries.
// WITHOUT ROWID stores rows in the primary key b-tree; a rowid table would
// keep a second copy of (hour, user_agent, model) in an auto index.
func (s *SQLiteStore) ensureUsageHourly() error {
	var exists int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'usage_hourly'`).Scan(&exists); err != nil {
		return err
	}
	if exists > 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
CREATE TABLE usage_hourly (
  hour INTEGER NOT NULL,
  user_agent TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  attempts INTEGER NOT NULL DEFAULT 0,
  requests INTEGER NOT NULL DEFAULT 0,
  success INTEGER NOT NULL DEFAULT 0,
  errors INTEGER NOT NULL DEFAULT 0,
  canceled INTEGER NOT NULL DEFAULT 0,
  incomplete INTEGER NOT NULL DEFAULT 0,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  cached_tokens INTEGER NOT NULL DEFAULT 0,
  last_seen_ms INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (hour, user_agent, model)
) WITHOUT ROWID`); err != nil {
		return err
	}
	if _, err := tx.Exec(`
INSERT INTO usage_hourly (hour, user_agent, model, attempts, requests, success, errors, canceled, incomplete,
  input_tokens, output_tokens, cached_tokens, last_seen_ms)
SELECT ts / 1000000000 / 3600 * 3600, user_agent, model,
  COUNT(*),
  SUM(CASE WHEN retried THEN 0 ELSE 1 END),
  SUM(CASE WHEN NOT retried AND outcome = 'completed' THEN 1 ELSE 0 END),
  SUM(CASE WHEN NOT retried AND outcome = 'error' THEN 1 ELSE 0 END),
  SUM(CASE WHEN NOT retried AND outcome = 'client_canceled' THEN 1 ELSE 0 END),
  SUM(CASE WHEN NOT retried AND NOT usage_complete THEN 1 ELSE 0 END),
  SUM(CASE WHEN usage_complete THEN input_tokens ELSE 0 END),
  SUM(CASE WHEN usage_complete THEN output_tokens ELSE 0 END),
  SUM(CASE WHEN usage_complete THEN cached_tokens ELSE 0 END),
  MAX(ts) / 1000000
FROM request_logs
GROUP BY 1, 2, 3`); err != nil {
		return fmt.Errorf("backfill usage_hourly: %w", err)
	}
	return tx.Commit()
}

func (s *SQLiteStore) backfillOutcome() error {
	_, err := s.db.Exec(`
UPDATE request_logs
SET usage_complete = CASE
      WHEN status_code < 400 AND error = ''
       AND (input_tokens <> 0 OR output_tokens <> 0 OR cached_tokens <> 0) THEN 1
      ELSE 0
    END,
    outcome = CASE
      WHEN LOWER(error) LIKE '%context canceled%' OR LOWER(error) LIKE '%context cancelled%' THEN 'client_canceled'
      WHEN status_code >= 400 OR error <> '' THEN 'error'
      WHEN input_tokens <> 0 OR output_tokens <> 0 OR cached_tokens <> 0 THEN 'completed'
      ELSE 'unknown'
    END
WHERE outcome = ''`)
	return err
}

// migrateTsToIntegerIfNeeded rewrites legacy TEXT RFC3339Nano ts columns to
// INTEGER Unix nanoseconds so retention comparisons are correct.
func (s *SQLiteStore) migrateTsToIntegerIfNeeded() error {
	decl, err := s.tsColumnType()
	if err != nil {
		return err
	}
	if decl == "" {
		return nil
	}
	// Already integer (or numeric affinity).
	if strings.EqualFold(decl, "INTEGER") || strings.EqualFold(decl, "INT") || strings.EqualFold(decl, "BIGINT") {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
CREATE TABLE request_logs_new (
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
);`); err != nil {
		return fmt.Errorf("create request_logs_new: %w", err)
	}

	rows, err := tx.Query(`SELECT id, request_id, ts, method, path, status_code, model, protocol, provider, upstream, user_agent, duration_ms, error, req_body, resp_body FROM request_logs`)
	if err != nil {
		return fmt.Errorf("select legacy rows: %w", err)
	}
	defer rows.Close()

	ins, err := tx.Prepare(`INSERT INTO request_logs_new (
  id, request_id, ts, method, path, status_code, model, protocol, provider, upstream,
  user_agent, duration_ms, error, req_body, resp_body
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer ins.Close()

	for rows.Next() {
		var (
			id, status, duration                                                int64
			requestID, tsRaw, method, path, model, protocol, provider, upstream string
			userAgent, errText, reqBody, respBody                               string
		)
		if err := rows.Scan(&id, &requestID, &tsRaw, &method, &path, &status, &model, &protocol, &provider, &upstream, &userAgent, &duration, &errText, &reqBody, &respBody); err != nil {
			return fmt.Errorf("scan legacy row: %w", err)
		}
		tsNano, err := parseLegacyTS(tsRaw)
		if err != nil {
			// Skip unparsable timestamps rather than failing the whole open.
			continue
		}
		if _, err := ins.Exec(id, requestID, tsNano, method, path, status, model, protocol, provider, upstream, userAgent, duration, errText, reqBody, respBody); err != nil {
			return fmt.Errorf("insert migrated row: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if _, err := tx.Exec(`DROP TABLE request_logs`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE request_logs_new RENAME TO request_logs`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_request_logs_ts ON request_logs(ts)`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) tsColumnType() (string, error) {
	rows, err := s.db.Query(`PRAGMA table_info(request_logs)`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return "", err
		}
		if name == "ts" {
			return strings.TrimSpace(ctype), nil
		}
	}
	return "", rows.Err()
}

func parseLegacyTS(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("empty ts")
	}
	// Already numeric (defensive).
	if isAllDigits(raw) {
		var n int64
		_, err := fmt.Sscan(raw, &n)
		return n, err
	}
	// Try common RFC3339 variants.
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02T15:04:05.999999999",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC().UnixNano(), nil
		}
	}
	return 0, fmt.Errorf("unrecognized ts %q", raw)
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (s *SQLiteStore) Insert(ctx context.Context, r Record) error {
	r = normalizeRecord(r)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO request_logs (
  request_id, ts, method, path, status_code, outcome, model, protocol, provider, upstream,
  user_agent, duration_ms, input_tokens, output_tokens, cached_tokens, usage_complete, retried, error, req_body, resp_body
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.RequestID,
		r.Timestamp.UTC().UnixNano(),
		r.Method,
		r.Path,
		r.StatusCode,
		r.Outcome,
		r.Model,
		r.Protocol,
		r.Provider,
		r.Upstream,
		r.UserAgent,
		r.DurationMS,
		r.InputTokens,
		r.OutputTokens,
		r.CachedTokens,
		r.UsageComplete,
		r.Retried,
		r.Error,
		r.ReqBody,
		r.RespBody,
	); err != nil {
		return err
	}
	d := usageDelta(r)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO usage_hourly (hour, user_agent, model, attempts, requests, success, errors, canceled, incomplete,
  input_tokens, output_tokens, cached_tokens, last_seen_ms)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (hour, user_agent, model) DO UPDATE SET
  attempts = attempts + excluded.attempts,
  requests = requests + excluded.requests,
  success = success + excluded.success,
  errors = errors + excluded.errors,
  canceled = canceled + excluded.canceled,
  incomplete = incomplete + excluded.incomplete,
  input_tokens = input_tokens + excluded.input_tokens,
  output_tokens = output_tokens + excluded.output_tokens,
  cached_tokens = cached_tokens + excluded.cached_tokens,
  last_seen_ms = MAX(last_seen_ms, excluded.last_seen_ms)`,
		d.Hour, d.UserAgent, d.Model, d.Attempts, d.Requests, d.Success, d.Errors, d.Canceled, d.Incomplete,
		d.InputTokens, d.OutputTokens, d.CachedTokens, d.LastSeenMS,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM request_logs WHERE ts < ?`, cutoff.UTC().UnixNano())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *SQLiteStore) StripBodies(ctx context.Context, after, before time.Time) (int64, error) {
	q := `UPDATE request_logs SET req_body = '', resp_body = '' WHERE ts < ? AND (req_body <> '' OR resp_body <> '')`
	args := []any{before.UTC().UnixNano()}
	if !after.IsZero() {
		q += ` AND ts >= ?`
		args = append(args, after.UTC().UnixNano())
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *SQLiteStore) Usage(ctx context.Context, from, to time.Time) ([]UsageBucket, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT hour, user_agent, model, attempts, requests, success, errors, canceled, incomplete,
  input_tokens, output_tokens, cached_tokens, last_seen_ms
FROM usage_hourly WHERE hour >= ? AND hour < ? ORDER BY hour`, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageBucket{}
	for rows.Next() {
		var b UsageBucket
		if err := rows.Scan(&b.Hour, &b.UserAgent, &b.Model, &b.Attempts, &b.Requests, &b.Success, &b.Errors, &b.Canceled, &b.Incomplete,
			&b.InputTokens, &b.OutputTokens, &b.CachedTokens, &b.LastSeenMS); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) Clear(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM request_logs`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

func (s *SQLiteStore) List(ctx context.Context, f ListFilter) ([]Record, int64, error) {
	f = normalizeListFilter(f)
	where, args := buildListWhere(f)

	var total int64
	countQ := `SELECT COUNT(*) FROM request_logs` + where
	if err := s.db.QueryRowContext(ctx, countQ, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := `SELECT id, request_id, ts, method, path, status_code, outcome, model, protocol, provider, upstream,
  user_agent, duration_ms, input_tokens, output_tokens, cached_tokens, usage_complete, retried, error, req_body, resp_body
FROM request_logs` + where + ` ORDER BY ts DESC, id DESC LIMIT ? OFFSET ?`
	listArgs := append(append([]any{}, args...), f.Limit, f.Offset)
	rows, err := s.db.QueryContext(ctx, q, listArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]Record, 0, f.Limit)
	for rows.Next() {
		var r Record
		var ts int64
		if err := rows.Scan(
			&r.ID, &r.RequestID, &ts, &r.Method, &r.Path, &r.StatusCode, &r.Outcome,
			&r.Model, &r.Protocol, &r.Provider, &r.Upstream,
			&r.UserAgent, &r.DurationMS, &r.InputTokens, &r.OutputTokens, &r.CachedTokens, &r.UsageComplete, &r.Retried, &r.Error, &r.ReqBody, &r.RespBody,
		); err != nil {
			return nil, 0, err
		}
		r.Timestamp = time.Unix(0, ts).UTC()
		items = append(items, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

func (s *SQLiteStore) Stats(ctx context.Context, f ListFilter) (Stats, error) {
	st := emptyStats()
	where, args := buildListWhere(f)
	row := s.db.QueryRowContext(ctx, `
SELECT COUNT(*),
       COALESCE(SUM(CASE WHEN outcome = 'completed' THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN outcome = 'error' THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN outcome = 'client_canceled' THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN usage_complete THEN 0 ELSE 1 END), 0),
       COALESCE(AVG(duration_ms), 0),
       COALESCE(SUM(CASE WHEN usage_complete THEN input_tokens ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN usage_complete THEN output_tokens ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN usage_complete THEN cached_tokens ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN usage_complete AND duration_ms > 0 THEN duration_ms ELSE 0 END), 0)
FROM request_logs`+where, args...)
	var totalDurationMS int64
	if err := row.Scan(&st.Total, &st.Success, &st.Errors, &st.Canceled, &st.UsageIncomplete, &st.AvgDurationMS, &st.InputTokens, &st.OutputTokens, &st.CachedTokens, &totalDurationMS); err != nil {
		return Stats{}, err
	}
	if totalDurationMS > 0 {
		st.OutputTPS = float64(st.OutputTokens) * 1000 / float64(totalDurationMS)
	}

	var err error
	if st.ByStatus, err = s.groupCount(ctx, `CAST(status_code AS TEXT)`, where, args); err != nil {
		return Stats{}, err
	}
	if st.ByModel, err = s.groupCount(ctx, `CASE WHEN model = '' THEN '(empty)' ELSE model END`, where, args); err != nil {
		return Stats{}, err
	}
	if st.ByUpstream, err = s.groupCount(ctx, `CASE WHEN upstream = '' THEN '(empty)' ELSE upstream END`, where, args); err != nil {
		return Stats{}, err
	}
	if st.ByProtocol, err = s.groupCount(ctx, `CASE WHEN protocol = '' THEN '(empty)' ELSE protocol END`, where, args); err != nil {
		return Stats{}, err
	}
	finalizeStats(&st)
	return st, nil
}

func (s *SQLiteStore) groupCount(ctx context.Context, expr, where string, args []any) ([]NameCount, error) {
	q := `SELECT ` + expr + ` AS name, COUNT(*) AS c FROM request_logs` + where + ` GROUP BY 1 ORDER BY c DESC, name ASC LIMIT 20`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]NameCount, 0, 8)
	for rows.Next() {
		var nc NameCount
		if err := rows.Scan(&nc.Name, &nc.Count); err != nil {
			return nil, err
		}
		out = append(out, nc)
	}
	return out, rows.Err()
}
