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
  error TEXT NOT NULL DEFAULT '',
  req_body TEXT NOT NULL DEFAULT '',
  resp_body TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_request_logs_ts ON request_logs(ts);
`)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
ALTER TABLE request_logs
  ADD COLUMN IF NOT EXISTS input_tokens BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS output_tokens BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS cached_tokens BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS outcome TEXT NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS usage_complete BOOLEAN NOT NULL DEFAULT FALSE;
`)
	if err != nil {
		return err
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
	return err
}

func (s *PostgresStore) Insert(ctx context.Context, r Record) error {
	r = normalizeRecord(r)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO request_logs (
  request_id, ts, method, path, status_code, outcome, model, protocol, provider, upstream,
  user_agent, duration_ms, input_tokens, output_tokens, cached_tokens, usage_complete, error, req_body, resp_body
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`,
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
		r.Error,
		r.ReqBody,
		r.RespBody,
	)
	return err
}

func (s *PostgresStore) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM request_logs WHERE ts < $1`, cutoff.UTC())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
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
  user_agent, duration_ms, input_tokens, output_tokens, cached_tokens, usage_complete, error, req_body, resp_body
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
			&r.UserAgent, &r.DurationMS, &r.InputTokens, &r.OutputTokens, &r.CachedTokens, &r.UsageComplete, &r.Error, &r.ReqBody, &r.RespBody,
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
