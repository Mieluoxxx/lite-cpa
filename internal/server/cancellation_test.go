package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Mieluoxxx/lite-cpa/internal/config"
	"github.com/Mieluoxxx/lite-cpa/internal/executor"
	"github.com/Mieluoxxx/lite-cpa/internal/registry"
	"github.com/Mieluoxxx/lite-cpa/internal/reqlog"
)

type cancellationWriter struct {
	header   http.Header
	status   int
	writeErr error
}

func (w *cancellationWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *cancellationWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *cancellationWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return len(p), nil
}

func TestForwardCancellationAccounting(t *testing.T) {
	t.Run("before response headers", func(t *testing.T) {
		srv, logger := newCancellationTestServer(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
		w := &cancellationWriter{}
		attempts := 0

		srv.forward(w, r, "cancel-before", "responses", "gpt-5", "gpt-5", []byte(`{"model":"gpt-5"}`), time.Now(),
			func(ctx context.Context, _ registry.UpstreamKey, _ string) (any, error) {
				attempts++
				return nil, ctx.Err()
			})

		if attempts != 1 || w.status != 0 {
			t.Fatalf("attempts=%d status=%d, want 1/0", attempts, w.status)
		}
		record := waitForCancellationRecords(t, logger, 1)[0]
		if record.StatusCode != 0 || record.Outcome != reqlog.OutcomeClientCanceled || record.UsageComplete {
			t.Fatalf("pre-header cancellation = %#v", record)
		}
	})

	t.Run("non-stream body write failure", func(t *testing.T) {
		srv, logger := newCancellationTestServer(t)
		r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		w := &cancellationWriter{writeErr: errors.New("client write failed")}
		response := []byte(`{"usage":{"input_tokens":10,"output_tokens":2}}`)

		srv.forward(w, r, "cancel-write", "responses", "gpt-5", "gpt-5", []byte(`{"model":"gpt-5"}`), time.Now(),
			func(context.Context, registry.UpstreamKey, string) (any, error) {
				return &executor.Result{Body: response}, nil
			})

		if w.status != http.StatusOK {
			t.Fatalf("wire status=%d want 200", w.status)
		}
		record := waitForCancellationRecords(t, logger, 1)[0]
		if record.StatusCode != http.StatusOK || record.Outcome != reqlog.OutcomeClientCanceled || record.UsageComplete {
			t.Fatalf("write cancellation = %#v", record)
		}
		if record.InputTokens != 10 || record.OutputTokens != 2 {
			t.Fatalf("observed usage=(%d,%d), want (10,2)", record.InputTokens, record.OutputTokens)
		}
	})

	t.Run("executor cancellation without client cancellation", func(t *testing.T) {
		srv, logger := newCancellationTestServer(t)
		r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		w := &cancellationWriter{}
		attempts := 0

		srv.forward(w, r, "upstream-cancel", "responses", "gpt-5", "gpt-5", []byte(`{"model":"gpt-5"}`), time.Now(),
			func(context.Context, registry.UpstreamKey, string) (any, error) {
				attempts++
				return nil, context.Canceled
			})

		if attempts != 2 || w.status != http.StatusBadGateway {
			t.Fatalf("attempts=%d status=%d, want 2/502", attempts, w.status)
		}
		for _, record := range waitForCancellationRecords(t, logger, 2) {
			if record.Outcome != reqlog.OutcomeError || record.StatusCode != http.StatusBadGateway {
				t.Fatalf("executor cancellation = %#v", record)
			}
		}
	})
}

func newCancellationTestServer(t *testing.T) (*Server, *reqlog.Logger) {
	t.Helper()
	logger, err := reqlog.Open(config.RequestLogConfig{
		Enabled: true, Backend: "sqlite", Retention: "1h",
		SQLite: config.SQLiteLogConfig{Path: filepath.Join(t.TempDir(), "requests.db")},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		RequestRetry: 2,
		OpenAIResponses: []config.Provider{
			{Name: "first", BaseURL: "http://first.invalid/v1", APIKey: "sk-first", Models: []config.ModelAlias{{Name: "gpt-5", Alias: "gpt-5"}}},
			{Name: "second", BaseURL: "http://second.invalid/v1", APIKey: "sk-second", Models: []config.ModelAlias{{Name: "gpt-5", Alias: "gpt-5"}}},
		},
	}
	srv := New(cfg, logger)
	t.Cleanup(func() {
		_ = srv.Shutdown(t.Context())
		_ = logger.Close()
	})
	return srv, logger
}

func waitForCancellationRecords(t *testing.T, logger *reqlog.Logger, want int) []reqlog.Record {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		logs, err := logger.List(t.Context(), reqlog.ListFilter{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(logs.Items) == want {
			return logs.Items
		}
		if time.Now().After(deadline) {
			t.Fatalf("request log not persisted: %#v", logs.Items)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
