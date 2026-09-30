package reqlog

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type PostgresStore struct {
	db *sql.DB
}

func OpenPostgres(dsn string) (*PostgresStore, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetConnMaxLifetime(time.Hour)
	s := &PostgresStore{db: db}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *PostgresStore) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS request_logs (
  id BIGSERIAL PRIMARY KEY,
  request_id TEXT NOT NULL DEFAULT '',
  ts TIMESTAMPTZ NOT NULL,
  method TEXT NOT NULL DEFAULT '',
  path TEXT NOT NULL DEFAULT '',
  status_code INTEGER NOT NULL DEFAULT 0,
  outcome TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  protocol TEXT NOT NULL DEFAULT '',
  provider TEXT NOT NULL DEFAULT '',
  upstream TEXT NOT NULL DEFAULT '',
  user_agent TEXT NOT NULL DEFAULT '',
  duration_ms BIGINT NOT NULL DEFAULT 0,
  input_tokens BIGINT NOT NULL DEFAULT 0,
  output_tokens BIGINT NOT NULL DEFAULT 0,
  cached_tokens BIGINT NOT NULL DEFAULT 0,
  usage_complete BOOLEAN NOT NULL DEFAULT FALSE,
  retried BOOLEAN NOT NULL DEFAULT FALSE,
  error TEXT NOT NULL DEFAULT '',
  req_body TEXT NOT NULL DEFAULT '',
  resp_body TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_request_logs_ts ON request_logs(ts);
`)
	if err != nil {
		return err
	}
	var hadRetried bool
	if err := s.db.QueryRowContext(ctx, `
SELECT EXISTS (
  SELECT 1 FROM information_schema.columns
  WHERE table_schema = current_schema() AND table_name = 'request_logs' AND column_name = 'retried'
)`).Scan(&hadRetried); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
ALTER TABLE request_logs
  ADD COLUMN IF NOT EXISTS input_tokens BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS output_tokens BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS cached_tokens BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS outcome TEXT NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS usage_complete BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN IF NOT EXISTS retried BOOLEAN NOT NULL DEFAULT FALSE;
`)
	if err != nil {
		return err
	}
	if !hadRetried {
		// Every row except the last one of a request_id was superseded by a later attempt.
		if _, err := s.db.ExecContext(ctx, `
UPDATE request_logs r SET retried = TRUE
FROM (
  SELECT request_id, MAX(id) AS last_id FROM request_logs
  WHERE request_id <> '' GROUP BY request_id HAVING COUNT(*) > 1
) g
WHERE r.request_id = g.request_id AND r.id < g.last_id`); err != nil {
			return fmt.Errorf("backfill retried: %w", err)
		}
	}
	_, err = s.db.ExecContext(ctx, `
UPDATE request_logs
SET usage_complete = status_code < 400 AND error = ''
                     AND (input_tokens <> 0 OR output_tokens <> 0 OR cached_tokens <> 0),
    outcome = CASE
      WHEN error ILIKE '%context canceled%' OR error ILIKE '%context cancelled%' THEN 'client_canceled'
      WHEN status_code >= 400 OR error <> '' THEN 'error'
      WHEN input_tokens <> 0 OR output_tokens <> 0 OR cached_tokens <> 0 THEN 'completed'
      ELSE 'unknown'
    END
WHERE outcome = ''`)
	if err != nil {
		return err
	}
	return s.ensureUsageHourly(ctx)
}

// ensureUsageHourly creates the rollup table and, when it is new, fills it
// from existing rows in the same transaction so a crash just retries.
func (s *PostgresStore) ensureUsageHourly(ctx context.Context) error {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT to_regclass('usage_hourly') IS NOT NULL`).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
CREATE TABLE usage_hourly (
  hour BIGINT NOT NULL,
  user_agent TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  attempts BIGINT NOT NULL DEFAULT 0,
  requests BIGINT NOT NULL DEFAULT 0,
  success BIGINT NOT NULL DEFAULT 0,
  errors BIGINT NOT NULL DEFAULT 0,
  canceled BIGINT NOT NULL DEFAULT 0,
  incomplete BIGINT NOT NULL DEFAULT 0,
  input_tokens BIGINT NOT NULL DEFAULT 0,
  output_tokens BIGINT NOT NULL DEFAULT 0,
  cached_tokens BIGINT NOT NULL DEFAULT 0,
  last_seen_ms BIGINT NOT NULL DEFAULT 0,
  PRIMARY KEY (hour, user_agent, model)
)`); err != nil {
		return err
	}
	// FLOOR before the cast: casting EXTRACT(EPOCH) rounds, which would push
	// the last half second of an hour into the next bucket.
	if _, err := tx.ExecContext(ctx, `
INSERT INTO usage_hourly (hour, user_agent, model, attempts, requests, success, errors, canceled, incomplete,
  input_tokens, output_tokens, cached_tokens, last_seen_ms)
SELECT FLOOR(EXTRACT(EPOCH FROM ts))::bigint / 3600 * 3600, user_agent, model,
  COUNT(*),
  SUM(CASE WHEN retried THEN 0 ELSE 1 END),
  SUM(CASE WHEN NOT retried AND outcome = 'completed' THEN 1 ELSE 0 END),
  SUM(CASE WHEN NOT retried AND outcome = 'error' THEN 1 ELSE 0 END),
  SUM(CASE WHEN NOT retried AND outcome = 'client_canceled' THEN 1 ELSE 0 END),
  SUM(CASE WHEN NOT retried AND NOT usage_complete THEN 1 ELSE 0 END),
  SUM(CASE WHEN usage_complete THEN input_tokens ELSE 0 END),
  SUM(CASE WHEN usage_complete THEN output_tokens ELSE 0 END),
  SUM(CASE WHEN usage_complete THEN cached_tokens ELSE 0 END),
  FLOOR(EXTRACT(EPOCH FROM MAX(ts)) * 1000)::bigint
FROM request_logs
GROUP BY 1, 2, 3`); err != nil {
		return fmt.Errorf("backfill usage_hourly: %w", err)
	}
	return tx.Commit()
}

func (s *PostgresStore) Insert(ctx context.Context, r Record) error {
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
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`,
		r.RequestID,
		r.Timestamp.UTC(),
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
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
ON CONFLICT (hour, user_agent, model) DO UPDATE SET
  attempts = usage_hourly.attempts + EXCLUDED.attempts,
  requests = usage_hourly.requests + EXCLUDED.requests,
  success = usage_hourly.success + EXCLUDED.success,
  errors = usage_hourly.errors + EXCLUDED.errors,
  canceled = usage_hourly.canceled + EXCLUDED.canceled,
  incomplete = usage_hourly.incomplete + EXCLUDED.incomplete,
  input_tokens = usage_hourly.input_tokens + EXCLUDED.input_tokens,
  output_tokens = usage_hourly.output_tokens + EXCLUDED.output_tokens,
  cached_tokens = usage_hourly.cached_tokens + EXCLUDED.cached_tokens,
  last_seen_ms = GREATEST(usage_hourly.last_seen_ms, EXCLUDED.last_seen_ms)`,
		d.Hour, d.UserAgent, d.Model, d.Attempts, d.Requests, d.Success, d.Errors, d.Canceled, d.Incomplete,
		d.InputTokens, d.OutputTokens, d.CachedTokens, d.LastSeenMS,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresStore) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM request_logs WHERE ts < $1`, cutoff.UTC())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *PostgresStore) StripBodies(ctx context.Context, after, before time.Time) (int64, error) {
	q := `UPDATE request_logs SET req_body = '', resp_body = '' WHERE ts < $1 AND (req_body <> '' OR resp_body <> '')`
	args := []any{before.UTC()}
	if !after.IsZero() {
		q += ` AND ts >= $2`
		args = append(args, after.UTC())
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *PostgresStore) Usage(ctx context.Context, from, to time.Time) ([]UsageBucket, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT hour, user_agent, model, attempts, requests, success, errors, canceled, incomplete,
  input_tokens, output_tokens, cached_tokens, last_seen_ms
FROM usage_hourly WHERE hour >= $1 AND hour < $2 ORDER BY hour`, from.Unix(), to.Unix())
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

func (s *PostgresStore) Clear(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM request_logs`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *PostgresStore) Close() error {
	return s.db.Close()
}

func (s *PostgresStore) List(ctx context.Context, f ListFilter) ([]Record, int64, error) {
	f = normalizeListFilter(f)
	where, args := buildListWhere(f, true)

	var total int64
	countQ := `SELECT COUNT(*) FROM request_logs` + where
	if err := s.db.QueryRowContext(ctx, countQ, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	limitIdx := len(args) + 1
	offsetIdx := len(args) + 2
	q := fmt.Sprintf(`SELECT id, request_id, ts, method, path, status_code, outcome, model, protocol, provider, upstream,
  user_agent, duration_ms, input_tokens, output_tokens, cached_tokens, usage_complete, retried, error, req_body, resp_body
FROM request_logs%s ORDER BY ts DESC, id DESC LIMIT $%d OFFSET $%d`, where, limitIdx, offsetIdx)
	listArgs := append(append([]any{}, args...), f.Limit, f.Offset)
	rows, err := s.db.QueryContext(ctx, q, listArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]Record, 0, f.Limit)
	for rows.Next() {
		var r Record
		if err := rows.Scan(
			&r.ID, &r.RequestID, &r.Timestamp, &r.Method, &r.Path, &r.StatusCode, &r.Outcome,
			&r.Model, &r.Protocol, &r.Provider, &r.Upstream,
			&r.UserAgent, &r.DurationMS, &r.InputTokens, &r.OutputTokens, &r.CachedTokens, &r.UsageComplete, &r.Retried, &r.Error, &r.ReqBody, &r.RespBody,
		); err != nil {
			return nil, 0, err
		}
		r.Timestamp = r.Timestamp.UTC()
		items = append(items, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

func (s *PostgresStore) Stats(ctx context.Context, f ListFilter) (Stats, error) {
	st := emptyStats()
	where, args := buildListWhere(f, true)
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
	if st.ByStatus, err = s.groupCount(ctx, `status_code::text`, where, args); err != nil {
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

func (s *PostgresStore) groupCount(ctx context.Context, expr, where string, args []any) ([]NameCount, error) {
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
