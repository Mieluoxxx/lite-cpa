package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mieluoxxx/lite-cpa/internal/access"
	"github.com/Mieluoxxx/lite-cpa/internal/affinity"
	"github.com/Mieluoxxx/lite-cpa/internal/config"
	"github.com/Mieluoxxx/lite-cpa/internal/executor"
	"github.com/Mieluoxxx/lite-cpa/internal/pool"
	"github.com/Mieluoxxx/lite-cpa/internal/registry"
	"github.com/Mieluoxxx/lite-cpa/internal/reqlog"
	"github.com/Mieluoxxx/lite-cpa/internal/thinking"
	"github.com/Mieluoxxx/lite-cpa/internal/translator"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

type Server struct {
	cfgMu    sync.RWMutex
	cfg      *config.Config
	reg      *registry.Registry
	selector *pool.Selector
	health   *pool.HealthState
	affinity *affinity.Manager
	auth     *access.Checker
	logger   *reqlog.Logger
	http     *http.Server

	maxBody atomic.Int64

	reloadMu sync.Mutex
	stateMu  sync.RWMutex
}

func New(cfg *config.Config, logger *reqlog.Logger) *Server {
	if logger == nil {
		logger = &reqlog.Logger{}
	}
	reg := pool.BuildRegistry(cfg)
	health := pool.NewHealthState()
	s := &Server{
		cfg:      cfg,
		reg:      reg,
		selector: pool.NewSelector(reg, cfg.RequestRetry, health),
		health:   health,
		affinity: affinity.New(cfg.ChannelAffinity),
		auth:     access.New(cfg.APIKeys),
		logger:   logger,
	}
	s.maxBody.Store(cfg.MaxBodyBytes)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /", s.handleRoot)
	mux.HandleFunc("GET /dashboard", s.handleDashboard)
	mux.HandleFunc("GET /dashboard.html", s.handleDashboard)
	mux.HandleFunc("GET /dashboard/", s.handleDashboardAsset)
	mux.HandleFunc("GET /api/logs", s.handleLogsList)
	mux.HandleFunc("DELETE /api/logs", s.handleLogsClear)
	mux.HandleFunc("GET /api/logs/stats", s.handleLogsStats)
	mux.HandleFunc("GET /api/usage", s.handleUsage)
	mux.HandleFunc("GET /api/affinity/stats", s.handleAffinityStats)
	mux.HandleFunc("GET /api/routing/stats", s.handleRoutingStats)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("POST /v1/responses", s.handleResponses)
	mux.HandleFunc("POST /v1/messages", s.handleMessages)
	mux.HandleFunc("POST /v1/images/generations", func(w http.ResponseWriter, r *http.Request) {
		s.handleImages(w, r, "generations")
	})
	mux.HandleFunc("POST /v1/images/edits", func(w http.ResponseWriter, r *http.Request) {
		s.handleImages(w, r, "edits")
	})
	handler := s.compress(s.auth.Middleware(s.limitBody(s.decodeBody(withRecover(mux)))))
	addr := net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port))
	s.http = &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return s
}

func (s *Server) ListenAndServe() error {
	log.Printf("lite-cpa listening on %s", s.http.Addr)
	return s.http.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s.affinity != nil {
		s.affinity.Close()
	}
	return s.http.Shutdown(ctx)
}

// Reload applies a newly loaded config in place.
//
// Hot-reloadable fields (api-keys, providers/models/keys, request-retry,
// channel-affinity, max-body-bytes, request-log.store-body) are always applied,
// even when the same save also touched immutable fields.
//
// host/port and request-log backend identity (enabled/backend/path/
// retention) are immutable at runtime: changes there are logged as a warning
// and the previous values stay active, but every reloadable field in the same
// save still takes effect. Invalid configs are rejected by config.Load first.
func (s *Server) Reload(cfg *config.Config) error {
	if cfg == nil {
		return fmt.Errorf("reload: nil config")
	}
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()

	s.cfgMu.RLock()
	old := s.cfg
	s.cfgMu.RUnlock()
	if old == nil {
		return fmt.Errorf("reload: server has no active config")
	}

	// Collect immutable-field drift; pin the old values into the stored config
	// so s.cfg keeps reflecting what is actually live, and the warning re-fires
	// on every subsequent reload instead of going silent after the first one.
	var deferred []string
	merged := *cfg
	if old.Host != cfg.Host || old.Port != cfg.Port {
		deferred = append(deferred, fmt.Sprintf("host/port (%s:%d -> %s:%d)",
			old.Host, old.Port, cfg.Host, cfg.Port))
		merged.Host = old.Host
		merged.Port = old.Port
	}
	if requestLogIdentity(old.RequestLog) != requestLogIdentity(cfg.RequestLog) {
		deferred = append(deferred, "request-log enabled/backend/path/retention/body-retention")
		storeBody := merged.RequestLog.StoreBody // store-body stays hot-reloadable
		merged.RequestLog = old.RequestLog
		merged.RequestLog.StoreBody = storeBody
	}

	nextReg := pool.BuildRegistry(&merged)
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.health.Reset()
	s.affinity.Reset()
	s.reg.ReplaceFrom(nextReg)
	s.selector.SetRetry(merged.RequestRetry)
	s.selector.ResetRoundRobin() // start fresh against rebuilt key pools
	s.auth.Replace(merged.APIKeys)
	s.affinity.Reconfigure(merged.ChannelAffinity)
	s.maxBody.Store(merged.MaxBodyBytes)

	s.cfgMu.Lock()
	s.cfg = &merged
	s.cfgMu.Unlock()

	log.Printf("config reloaded: models=%d retry=%d api-keys=%d",
		len(s.reg.List()), merged.RequestRetry, len(merged.APIKeys))
	if len(deferred) > 0 {
		log.Printf("config reload: %s require process restart; previous values kept",
			strings.Join(deferred, ", "))
	}
	return nil
}

func requestLogIdentity(c config.RequestLogConfig) string {
	return fmt.Sprintf("%t|%s|%s|%s|%s",
		c.Enabled,
		strings.ToLower(strings.TrimSpace(c.Backend)),
		strings.TrimSpace(c.SQLite.Path),
		strings.TrimSpace(c.Retention),
		strings.TrimSpace(c.BodyRetention),
	)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) handleRoot(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"name":"lite-cpa","endpoints":["GET /dashboard","GET /dashboard.html","GET /api/logs","DELETE /api/logs","GET /api/logs/stats","GET /api/usage","GET /api/affinity/stats","GET /api/routing/stats","GET /v1/models","POST /v1/chat/completions","POST /v1/responses","POST /v1/messages","POST /v1/images/generations","POST /v1/images/edits"]}`))
}

func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	models := s.reg.List()
	data := make([]map[string]any, 0, len(models))
	for _, m := range models {
		data = append(data, map[string]any{
			"id": m.ID, "object": "model", "created": m.Created, "owned_by": m.OwnedBy,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	s.handleProxy(w, r, translator.FormatOpenAI)
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	s.handleProxy(w, r, translator.FormatOpenAIResponse)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	s.handleProxy(w, r, translator.FormatClaude)
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request, source translator.Format) {
	start := time.Now()
	reqID := uuid.NewString()
	protocol := protocolOf(source)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("read body: %v", err))
		s.logReq(reqID, r, protocol, "", "", "", http.StatusBadRequest, reqlog.OutcomeError, start, err.Error(), body, nil)
		return
	}
	model := gjson.GetBytes(body, "model").String()
	if model == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		s.logReq(reqID, r, protocol, "", "", "", http.StatusBadRequest, reqlog.OutcomeError, start, "model is required", body, nil)
		return
	}
	baseModel := thinking.ParseSuffix(model).ModelName
	stream := gjson.GetBytes(body, "stream").Bool()

	resolveName := model
	if _, _, ok := s.reg.Resolve(model); !ok {
		resolveName = baseModel
	}

	losses := translator.NewLossRecorder()
	s.forward(w, r, reqID, protocol, model, resolveName, body, start,
		func(ctx context.Context, key registry.UpstreamKey, upstreamModel string) (any, error) {
			return executor.Execute(translator.WithLossRecorder(ctx, losses), key, upstreamModel, source, body, stream)
		})
	s.logConversionLoss(reqID, source, losses)
}

// logConversionLoss reports what the conversion to the upstream protocol could
// not carry. The client never sees it, so stderr is the only place it shows up:
// one summary line per request, plus per-field detail when debug is on.
func (s *Server) logConversionLoss(reqID string, source translator.Format, losses *translator.LossRecorder) {
	diags := losses.Diagnostics()
	if len(diags) == 0 {
		return
	}
	log.Printf("conversion loss req=%s from=%s %s", reqID, source, losses.Summary())
	if !s.debugEnabled() {
		return
	}
	for _, diag := range diags {
		log.Printf("conversion loss req=%s severity=%s code=%s path=%s %s", reqID, diag.Severity, diag.Code, diag.Path, diag.Message)
	}
}

func (s *Server) logReq(id string, r *http.Request, protocol, model, provider, upstream string, status int, outcome string, start time.Time, errMsg string, reqBody, respBody []byte, usage ...tokenUsage) {
	if rec, ok := s.newLogRecord(id, r, protocol, model, provider, upstream, status, outcome, start, errMsg, reqBody, respBody, usage...); ok {
		s.logger.Record(rec)
	}
}

// newLogRecord builds a request-log row; ok is false when logging is off.
func (s *Server) newLogRecord(id string, r *http.Request, protocol, model, provider, upstream string, status int, outcome string, start time.Time, errMsg string, reqBody, respBody []byte, usage ...tokenUsage) (reqlog.Record, bool) {
	if s.logger == nil || !s.logger.Enabled() {
		return reqlog.Record{}, false
	}
	rec := reqlog.Record{
		RequestID:  id,
		Timestamp:  start,
		Method:     r.Method,
		Path:       r.URL.Path,
		StatusCode: status,
		Outcome:    outcome,
		Model:      model,
		Protocol:   protocol,
		Provider:   provider,
		Upstream:   upstream,
		UserAgent:  r.UserAgent(),
		DurationMS: time.Since(start).Milliseconds(),
		Error:      errMsg,
	}
	if len(usage) > 0 {
		rec.InputTokens = usage[0].inputTokens
		rec.OutputTokens = usage[0].outputTokens
		rec.CachedTokens = usage[0].cachedTokens
		rec.UsageComplete = outcome == reqlog.OutcomeCompleted && usage[0].complete()
	} else if outcome == reqlog.OutcomeCompleted && protocol == "openai-image" {
		rec.UsageComplete = true
	}
	if s.currentCfg().RequestLog.StoreBody {
		// Cap stored bodies to keep memory/disk bounded.
		rec.ReqBody = truncate(string(reqBody), 64<<10)
		rec.RespBody = truncate(string(respBody), 64<<10)
	}
	return rec, true
}

// requestLog orders the rows of one client request. A retriable failure is
// held until the next attempt starts (written as retried) or the request ends
// (written as the final row), so usage stats see exactly one final row per
// request while the failed attempt stays visible in the log.
type requestLog struct {
	logger  *reqlog.Logger
	pending *reqlog.Record
}

func (l *requestLog) hold(rec reqlog.Record, ok bool) {
	l.flush(true)
	if ok {
		l.pending = &rec
	}
}

func (l *requestLog) emit(rec reqlog.Record, ok bool) {
	l.flush(true)
	if ok {
		l.logger.Record(rec)
	}
}

func (l *requestLog) flush(retried bool) {
	if l.pending == nil {
		return
	}
	rec := *l.pending
	l.pending = nil
	rec.Retried = retried
	l.logger.Record(rec)
}

func (s *Server) observeAttemptWithTokens(reqID string, attempt int, model string, key registry.UpstreamKey, lease pool.Lease, status int, outcome pool.AttemptOutcome, startedAt, firstByteAt time.Time, outputTokens int64, outputKnown bool, retryAfter ...time.Duration) {
	if s.selector == nil {
		return
	}
	var firstChunkMS int64
	if !firstByteAt.IsZero() {
		firstChunkMS = firstByteAt.Sub(startedAt).Milliseconds()
	}
	var retryAfterDuration time.Duration
	if len(retryAfter) > 0 {
		retryAfterDuration = retryAfter[0]
	}
	s.selector.Finish(lease, pool.AttemptResult{
		RequestID:         reqID,
		Attempt:           attempt + 1,
		Model:             model,
		KeyID:             key.ID,
		Provider:          key.Provider,
		Upstream:          key.Name,
		ActualModel:       key.UpstreamModelOr(model),
		Status:            status,
		Outcome:           outcome,
		DurationMS:        time.Since(startedAt).Milliseconds(),
		FirstChunkMS:      firstChunkMS,
		OutputTokens:      outputTokens,
		FirstChunkKnown:   !firstByteAt.IsZero(),
		OutputTokensKnown: outputKnown,
		RetryAfter:        retryAfterDuration,
		ProviderScoped:    key.FailoverMode == "provider" && (outcome == pool.OutcomeUpstream || outcome == pool.OutcomeTimeout),
		StartedAt:         startedAt,
	})
}

func classifyAttempt(err error, status int) pool.AttemptOutcome {
	if err == nil {
		return pool.OutcomeSuccess
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return pool.OutcomeAuth
	}
	if status == http.StatusTooManyRequests {
		return pool.OutcomeRateLimited
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return pool.OutcomeTimeout
	}
	if status >= http.StatusInternalServerError || status == 0 {
		return pool.OutcomeUpstream
	}
	return pool.OutcomeRequestError
}

func protocolOf(source translator.Format) string {
	switch source {
	case translator.FormatOpenAI:
		return "chat"
	case translator.FormatOpenAIResponse:
		return "responses"
	case translator.FormatClaude:
		return "claude"
	default:
		return source.String()
	}
}

func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max]
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAPIError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": typ},
	})
}

// normalizeErrorBody re-wraps a flat upstream error object under "error".
// OpenAI/Anthropic SDK clients only read body.error; some upstreams return
// {"object":"error","message":...} at the top level, which clients then render
// as "<status> status code (no body)". Every other shape passes through as is.
// The wrap is byte-wise on purpose: decoding and re-encoding the body would send
// large integers through float64 and change them.
func normalizeErrorBody(body string) []byte {
	raw := []byte(body)
	msg := gjson.Get(body, "message")
	if !json.Valid(raw) || !msg.Exists() || msg.Type != gjson.String || gjson.Get(body, "error").Exists() {
		return raw
	}
	return append(append([]byte(`{"error":`), raw...), '}')
}

func (s *Server) currentCfg() *config.Config {
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	return cfg
}

func (s *Server) debugEnabled() bool {
	cfg := s.currentCfg()
	return cfg != nil && cfg.Debug
}

func (s *Server) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		max := s.maxBody.Load()
		if r.Body != nil && max > 0 {
			r.Body = http.MaxBytesReader(w, r.Body, max)
		}
		next.ServeHTTP(w, r)
	})
}

func withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic: %v", rec)
				writeAPIError(w, http.StatusInternalServerError, "server_error", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// forward runs the shared upstream-selection + retry + write-back loop used by
// every proxy handler. execute is the single handler-specific step: chat /
// responses translation for handleProxy, raw image passthrough for handleImages.
// handleImages proxies OpenAI Images API requests (/v1/images/generations,
// /v1/images/edits) to an upstream that already speaks the standard Images
// API. The body — JSON or multipart — is forwarded verbatim; only model and
// stream are read out (from JSON or the multipart form) for routing and
// billing. imageEndpoint is "generations" or "edits".
func (s *Server) handleImages(w http.ResponseWriter, r *http.Request, imageEndpoint string) {
	start := time.Now()
	reqID := uuid.NewString()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("read body: %v", err))
		s.logReq(reqID, r, "openai-image", "", "", "", http.StatusBadRequest, reqlog.OutcomeError, start, err.Error(), body, nil)
		return
	}

	model, stream, err := extractImageMeta(body, r.Header.Get("Content-Type"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		s.logReq(reqID, r, "openai-image", "", "", "", http.StatusBadRequest, reqlog.OutcomeError, start, err.Error(), body, nil)
		return
	}
	if model == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		s.logReq(reqID, r, "openai-image", "", "", "", http.StatusBadRequest, reqlog.OutcomeError, start, "model is required", body, nil)
		return
	}

	resolveName := model
	if _, _, ok := s.reg.Resolve(model); !ok {
		if base := thinking.ParseSuffix(model).ModelName; base != "" {
			resolveName = base
		}
	}
	contentType := r.Header.Get("Content-Type")

	s.forward(w, r, reqID, "openai-image", model, resolveName, body, start,
		func(ctx context.Context, key registry.UpstreamKey, upstreamModel string) (any, error) {
			return executor.ExecuteImage(ctx, key, body, contentType, imageEndpoint, stream)
		})
}

// extractImageMeta pulls the model and stream flag from an Images API request.
// /v1/images/generations is JSON; /v1/images/edits may be JSON or multipart.
// For multipart the body is otherwise left untouched — it is forwarded verbatim
// with its boundary — and only the routing-relevant fields are read.
func extractImageMeta(body []byte, contentType string) (model string, stream bool, err error) {
	mediaType, params, parseErr := mime.ParseMediaType(contentType)
	if parseErr == nil && strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return "", false, fmt.Errorf("multipart boundary is required")
		}
		form, errForm := multipart.NewReader(bytes.NewReader(body), boundary).ReadForm(32 << 20)
		if errForm != nil {
			return "", false, fmt.Errorf("parse multipart form: %v", errForm)
		}
		defer form.RemoveAll()
		if v := form.Value["model"]; len(v) > 0 {
			model = strings.TrimSpace(v[0])
		}
		if v := form.Value["stream"]; len(v) > 0 {
			stream = strings.EqualFold(strings.TrimSpace(v[0]), "true")
		}
		return model, stream, nil
	}
	model = gjson.GetBytes(body, "model").String()
	stream = gjson.GetBytes(body, "stream").Bool()
	return model, stream, nil
}
