package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"strconv"
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
	health.ConfigureRouting(cfg.Routing.Strategy, cfg.Routing.HalfLifeDuration(), cfg.Routing.Shadow)
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
	handler := s.auth.Middleware(s.limitBody(withRecover(mux)))
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
// host/port and request-log backend identity (enabled/backend/path/dsn/
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
		deferred = append(deferred, "request-log enabled/backend/path/dsn/retention")
		storeBody := merged.RequestLog.StoreBody // store-body stays hot-reloadable
		merged.RequestLog = old.RequestLog
		merged.RequestLog.StoreBody = storeBody
	}

	nextReg := pool.BuildRegistry(&merged)
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.health.Reset()
	s.health.ConfigureRouting(merged.Routing.Strategy, merged.Routing.HalfLifeDuration(), merged.Routing.Shadow)
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
		strings.TrimSpace(c.Postgres.DSN),
		strings.TrimSpace(c.Retention),
	)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) handleRoot(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"name":"lite-cpa","endpoints":["GET /dashboard","GET /dashboard.html","GET /api/logs","DELETE /api/logs","GET /api/logs/stats","GET /api/affinity/stats","GET /api/routing/stats","GET /v1/models","POST /v1/chat/completions","POST /v1/responses","POST /v1/messages","POST /v1/images/generations","POST /v1/images/edits"]}`))
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

	s.forward(w, r, reqID, protocol, model, resolveName, body, start,
		func(ctx context.Context, key registry.UpstreamKey, upstreamModel string) (any, error) {
			return executor.Execute(ctx, key, upstreamModel, source, body, stream)
		})
}

func (s *Server) logReq(id string, r *http.Request, protocol, model, provider, upstream string, status int, outcome string, start time.Time, errMsg string, reqBody, respBody []byte, usage ...tokenUsage) {
	if s.logger == nil || !s.logger.Enabled() {
		return
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
	s.logger.Record(rec)
}

func (s *Server) observeAttempt(reqID string, attempt int, model string, key registry.UpstreamKey, lease pool.Lease, status int, outcome pool.AttemptOutcome, startedAt, firstByteAt time.Time, retryAfter ...time.Duration) {
	s.observeAttemptWithTokens(reqID, attempt, model, key, lease, status, outcome, startedAt, firstByteAt, 0, false, retryAfter...)
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
	actualModel := model
	if key.Headers != nil && key.Headers["x-lite-upstream-model"] != "" {
		actualModel = key.Headers["x-lite-upstream-model"]
	}
	s.selector.Finish(lease, pool.AttemptResult{
		RequestID:         reqID,
		Attempt:           attempt + 1,
		Model:             model,
		KeyID:             key.ID,
		Provider:          key.Provider,
		Upstream:          key.Name,
		ActualModel:       actualModel,
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
func (s *Server) forward(
	w http.ResponseWriter, r *http.Request,
	reqID, protocol, model, resolveName string,
	body []byte, start time.Time,
	execute func(ctx context.Context, key registry.UpstreamKey, upstreamModel string) (any, error),
) {
	s.stateMu.RLock()
	requestGeneration := s.health.Generation()
	aff := s.affinity.Lookup(resolveName, r.URL.Path, r.Header, body)
	if aff.Found {
		if s.debugEnabled() {
			log.Printf("affinity hit rule=%s key=%s", aff.RuleName, aff.KeyID)
		}
	}

	tried := make(map[string]struct{})
	skipSuppliers := make(map[string]struct{})
	preferSupplier := ""
	// Resolve the sticky (preferred) key once. Skip-retry only applies when the
	// pinned key is actually usable: after a reload it may have vanished, and
	// the request then falls back to normal selection with a full retry budget.
	var preferredKey registry.UpstreamKey
	preferredUsable := false
	var preferredLease pool.Lease
	if aff.Found {
		if _, keys, ok := s.reg.Resolve(resolveName); ok {
			if k, ok := affinity.ResolvePreferred(keys, aff.KeyID, tried); ok {
				if lease, admitted := s.selector.Acquire(resolveName, k); admitted {
					preferredKey = k
					preferredLease = lease
					preferredUsable = true
				}
			}
		}
	}
	maxAttempts := s.selector.MaxAttempts(resolveName)
	s.stateMu.RUnlock()
	if aff.Found && !preferredUsable {
		s.affinity.MarkPreferredUnavailable()
	}
	recordAffinity := func(key registry.UpstreamKey) {
		s.stateMu.RLock()
		defer s.stateMu.RUnlock()
		if requestGeneration != s.health.Generation() || !aff.Matched || aff.CacheKey == "" {
			return
		}
		if s.currentCfg().ChannelAffinity.SwitchOnSuccessOrDefault() || !aff.Found || aff.KeyID == key.ID {
			s.affinity.Record(aff.CacheKey, key.ID, aff.TTL)
			if s.debugEnabled() {
				log.Printf("affinity recorded rule=%s key=%s", aff.RuleName, key.ID)
			}
		}
	}
	clearAffinity := func(keyID string) {
		s.stateMu.RLock()
		defer s.stateMu.RUnlock()
		if requestGeneration != s.health.Generation() || !aff.Matched || aff.CacheKey == "" || keyID != aff.KeyID {
			return
		}
		s.affinity.Clear(aff.CacheKey)
		if s.debugEnabled() {
			log.Printf("affinity cleared rule=%s key=%s", aff.RuleName, keyID)
		}
	}
	if preferredUsable && aff.SkipRetry {
		maxAttempts = 1
	}
	var lastErr error
	var availabilityErr *pool.AvailabilityError
	lastErrLogged := false
	var lastKey registry.UpstreamKey
	for attempt := range maxAttempts {
		var key registry.UpstreamKey
		var upstreamModel string
		lease := pool.Lease{}
		var pickErr error

		if attempt == 0 && preferredUsable {
			key = preferredKey
			upstreamModel = resolveName
			if preferredKey.Headers != nil {
				if m := preferredKey.Headers["x-lite-upstream-model"]; m != "" {
					upstreamModel = m
				}
			}
			preferSupplier = preferredKey.Name
		}
		if key.ID == "" {
			s.stateMu.RLock()
			if attempt == 0 && !preferredUsable {
				key, upstreamModel, lease, pickErr = s.selector.PickWithLeaseExploring(resolveName, tried, preferSupplier, skipSuppliers)
			} else {
				key, upstreamModel, lease, pickErr = s.selector.PickWithLease(resolveName, tried, preferSupplier, skipSuppliers)
			}
			s.stateMu.RUnlock()
			if pickErr != nil {
				lastErr = pickErr
				var ae *pool.AvailabilityError
				if errors.As(pickErr, &ae) {
					availabilityErr = ae
				}
				lastErrLogged = false
				break
			}
			if preferSupplier == "" {
				preferSupplier = key.Name
			}
		} else {
			lease = preferredLease
			preferredLease = pool.Lease{}
		}
		tried[key.ID] = struct{}{}
		lastKey = key

		if thinking.ParseSuffix(model).HasSuffix {
			suffix := thinking.ParseSuffix(model).RawSuffix
			upstreamModel = thinking.ParseSuffix(upstreamModel).ModelName + "(" + suffix + ")"
		}

		attemptStart := time.Now()
		attemptCtx, cancelAttempt := context.WithCancel(r.Context())
		leasePtr := &lease
		defer func(l *pool.Lease, cancel context.CancelFunc) {
			cancel()
			s.selector.Release(*l)
		}(leasePtr, cancelAttempt)
		result, err := execute(attemptCtx, key, upstreamModel)
		if err != nil {
			cancelAttempt()
			if ctxErr := r.Context().Err(); ctxErr != nil {
				s.observeAttempt(reqID, attempt, resolveName, key, lease, 0, pool.OutcomeCanceled, attemptStart, time.Time{})
				s.logReq(reqID, r, protocol, model, key.Provider, key.Name, 0, reqlog.OutcomeClientCanceled, attemptStart, ctxErr.Error(), body, nil)
				return
			}
			lastErr = err
			lastErrLogged = false
			if se, ok := err.(executor.StatusError); ok {
				if se.Code == 401 || se.Code == 403 || se.Code == 429 || se.Code >= 500 {
					clearAffinity(key.ID)
					s.observeAttempt(reqID, attempt, resolveName, key, lease, se.Code, classifyAttempt(err, se.Code), attemptStart, time.Time{}, se.RetryAfter)
					s.logReq(reqID, r, protocol, model, key.Provider, key.Name, se.Code, reqlog.OutcomeError, attemptStart, se.Error(), body, nil)
					lastErrLogged = true
					if s.debugEnabled() {
						log.Printf("upstream %s/%s failed status=%d, rotating (mode=%s)", key.Name, key.ID, se.Code, key.FailoverMode)
					}
					if aff.Found && aff.SkipRetry && key.ID == aff.KeyID {
						break
					}
					if key.FailoverMode == "provider" {
						skipSuppliers[key.Name] = struct{}{}
						if preferSupplier == key.Name {
							preferSupplier = ""
						}
					}
					continue
				}
				s.observeAttempt(reqID, attempt, resolveName, key, lease, se.Code, pool.OutcomeRequestError, attemptStart, time.Time{})
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(se.Code)
				_, _ = w.Write([]byte(se.Body))
				s.logReq(reqID, r, protocol, model, key.Provider, key.Name, se.Code, reqlog.OutcomeError, start, se.Error(), body, []byte(se.Body))
				return
			}
			clearAffinity(key.ID)
			s.observeAttempt(reqID, attempt, resolveName, key, lease, http.StatusBadGateway, classifyAttempt(err, 0), attemptStart, time.Time{})
			s.logReq(reqID, r, protocol, model, key.Provider, key.Name, http.StatusBadGateway, reqlog.OutcomeError, attemptStart, err.Error(), body, nil)
			lastErrLogged = true
			if aff.Found && aff.SkipRetry && key.ID == aff.KeyID {
				break
			}
			if key.FailoverMode == "provider" {
				skipSuppliers[key.Name] = struct{}{}
				if preferSupplier == key.Name {
					preferSupplier = ""
				}
			}
			continue
		}
		if ctxErr := r.Context().Err(); ctxErr != nil {
			cancelAttempt()
			s.observeAttempt(reqID, attempt, resolveName, key, lease, 0, pool.OutcomeCanceled, attemptStart, time.Time{})
			s.logReq(reqID, r, protocol, model, key.Provider, key.Name, 0, reqlog.OutcomeClientCanceled, attemptStart, ctxErr.Error(), body, nil)
			return
		}

		switch v := result.(type) {
		case *executor.Result:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, writeErr := w.Write(v.Body)
			usage := usageFromResponse(v.Body)
			outcome := reqlog.OutcomeCompleted
			errMsg := ""
			if writeErr != nil {
				outcome = reqlog.OutcomeClientCanceled
				errMsg = writeErr.Error()
			} else if ctxErr := r.Context().Err(); ctxErr != nil {
				outcome = reqlog.OutcomeClientCanceled
				errMsg = ctxErr.Error()
			}
			if outcome == reqlog.OutcomeCompleted {
				s.observeAttemptWithTokens(reqID, attempt, resolveName, key, lease, v.Status, pool.OutcomeSuccess, attemptStart, time.Time{}, usage.outputTokens, usage.outputSeen)
				recordAffinity(key)
			} else {
				s.observeAttempt(reqID, attempt, resolveName, key, lease, v.Status, pool.OutcomeCanceled, attemptStart, time.Time{})
			}
			cancelAttempt()
			s.logReq(reqID, r, protocol, model, key.Provider, key.Name, http.StatusOK, outcome, start, errMsg, body, v.Body, usage)
			return
		case *executor.StreamResult:
			flusher, ok := w.(http.Flusher)
			if !ok {
				cancelAttempt()
				s.observeAttempt(reqID, attempt, resolveName, key, lease, http.StatusInternalServerError, pool.OutcomeRequestError, attemptStart, time.Time{})
				writeAPIError(w, http.StatusInternalServerError, "server_error", "streaming not supported")
				s.logReq(reqID, r, protocol, model, key.Provider, key.Name, http.StatusInternalServerError, reqlog.OutcomeError, start, "streaming not supported", body, nil)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.WriteHeader(http.StatusOK)
			flusher.Flush()
			var streamErr string
			clientCanceled := false
			streamAborted := false
			var firstByteAt time.Time
			var usage tokenUsage
			streamDone := false
			for !streamDone {
				select {
				case <-r.Context().Done():
					clientCanceled = true
					streamAborted = true
					streamErr = r.Context().Err().Error()
					cancelAttempt()
					streamDone = true
				case chunk, ok := <-v.Chunks:
					if !ok {
						streamDone = true
						continue
					}
					if chunk.Err != nil {
						streamAborted = true
						cancelAttempt()
						if s.debugEnabled() {
							log.Printf("stream error: %v", chunk.Err)
						}
						streamErr = chunk.Err.Error()
						streamDone = true
						continue
					}
					if chunk.LogError != "" && streamErr == "" {
						streamErr = chunk.LogError
					}
					if len(chunk.Payload) == 0 {
						continue
					}
					if firstByteAt.IsZero() {
						firstByteAt = time.Now()
					}
					usage.mergePayload(chunk.Payload)
					if _, err := w.Write(chunk.Payload); err != nil {
						streamErr = err.Error()
						clientCanceled = true
						streamAborted = true
						cancelAttempt()
						streamDone = true
						continue
					}
					flusher.Flush()
				}
			}
			if ctxErr := r.Context().Err(); ctxErr != nil {
				clientCanceled = true
				streamErr = ctxErr.Error()
			}
			completion := executor.StreamFailed
			if !streamAborted && !clientCanceled {
				if v.Complete != nil {
					select {
					case completion = <-v.Complete:
					case <-r.Context().Done():
						clientCanceled = true
						streamAborted = true
						streamErr = r.Context().Err().Error()
					}
				} else {
					streamErr = "upstream stream ended without a completion status"
				}
				if completion == executor.StreamIncomplete && streamErr == "" {
					streamErr = "upstream stream ended with an incomplete response"
				}
				if completion != executor.StreamCompleted && completion != executor.StreamIncomplete && streamErr == "" {
					streamErr = "upstream stream ended before completion"
				}
			}
			cancelAttempt()
			outcome := reqlog.OutcomeCompleted
			if clientCanceled {
				outcome = reqlog.OutcomeClientCanceled
			} else if streamErr != "" {
				outcome = reqlog.OutcomeError
			}
			if outcome == reqlog.OutcomeCompleted && completion == executor.StreamCompleted {
				s.observeAttemptWithTokens(reqID, attempt, resolveName, key, lease, v.Status, pool.OutcomeSuccess, attemptStart, firstByteAt, usage.outputTokens, usage.outputSeen)
				recordAffinity(key)
			} else if clientCanceled {
				s.observeAttemptWithTokens(reqID, attempt, resolveName, key, lease, v.Status, pool.OutcomeCanceled, attemptStart, firstByteAt, usage.outputTokens, usage.outputSeen)
			} else {
				attemptOutcome := pool.OutcomeUpstream
				if completion == executor.StreamIncomplete {
					attemptOutcome = pool.OutcomeRequestError
				}
				if !clientCanceled {
					clearAffinity(key.ID)
				}
				s.observeAttemptWithTokens(reqID, attempt, resolveName, key, lease, v.Status, attemptOutcome, attemptStart, firstByteAt, usage.outputTokens, usage.outputSeen)
			}
			s.logReq(reqID, r, protocol, model, key.Provider, key.Name, http.StatusOK, outcome, start, streamErr, body, nil, usage)
			return
		default:
			cancelAttempt()
			s.observeAttempt(reqID, attempt, resolveName, key, lease, http.StatusBadGateway, pool.OutcomeUpstream, attemptStart, time.Time{})
			lastErr = fmt.Errorf("unexpected executor result type %T", result)
			lastErrLogged = false
		}
	}
	if availabilityErr != nil {
		seconds := int64(math.Ceil(time.Until(availabilityErr.RetryAt).Seconds()))
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
		msg := fmt.Sprintf("all upstream credentials are cooling down for model %s", availabilityErr.Model)
		writeAPIError(w, http.StatusTooManyRequests, "rate_limit_error", msg)
		s.logReq(reqID, r, protocol, model, lastKey.Provider, lastKey.Name, http.StatusTooManyRequests, reqlog.OutcomeError, start, msg, body, nil)
		return
	}

	if lastErr != nil {
		if se, ok := lastErr.(executor.StatusError); ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(se.Code)
			_, _ = w.Write([]byte(se.Body))
			if !lastErrLogged {
				s.logReq(reqID, r, protocol, model, lastKey.Provider, lastKey.Name, se.Code, reqlog.OutcomeError, start, se.Error(), body, []byte(se.Body))
			}
			return
		}
		msg := lastErr.Error()
		code := http.StatusBadGateway
		if strings.Contains(msg, "model not found") {
			code = http.StatusNotFound
		}
		writeAPIError(w, code, "server_error", msg)
		if !lastErrLogged {
			s.logReq(reqID, r, protocol, model, lastKey.Provider, lastKey.Name, code, reqlog.OutcomeError, start, msg, body, nil)
		}
		return
	}
	writeAPIError(w, http.StatusBadGateway, "server_error", "all upstream credentials failed")
	s.logReq(reqID, r, protocol, model, lastKey.Provider, lastKey.Name, http.StatusBadGateway, reqlog.OutcomeError, start, "all upstream credentials failed", body, nil)
}

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
