package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Mieluoxxx/lite-cpa/internal/affinity"
	"github.com/Mieluoxxx/lite-cpa/internal/executor"
	"github.com/Mieluoxxx/lite-cpa/internal/pool"
	"github.com/Mieluoxxx/lite-cpa/internal/registry"
	"github.com/Mieluoxxx/lite-cpa/internal/reqlog"
	"github.com/Mieluoxxx/lite-cpa/internal/thinking"
	"github.com/tidwall/gjson"
)

// forwardRequest is the state shared by every upstream attempt of one client
// request.
type forwardRequest struct {
	s           *Server
	w           http.ResponseWriter
	r           *http.Request
	reqID       string
	protocol    string
	model       string
	resolveName string
	body        []byte
	start       time.Time
	rows        *requestLog
	aff         affinity.Match
	generation  uint64
	keepalive   *streamKeepalive
	committed   bool
}

// upstreamAttempt is one dispatch to one upstream key.
type upstreamAttempt struct {
	n      int
	key    registry.UpstreamKey
	lease  pool.Lease
	start  time.Time
	cancel context.CancelFunc
}

func (s *Server) forward(
	w http.ResponseWriter, r *http.Request,
	reqID, protocol, model, resolveName string,
	body []byte, start time.Time,
	execute func(ctx context.Context, key registry.UpstreamKey, upstreamModel string) (any, error),
) {
	f := &forwardRequest{
		s: s, w: w, r: r,
		reqID: reqID, protocol: protocol, model: model, resolveName: resolveName,
		body: body, start: start,
		rows: &requestLog{logger: s.logger},
	}
	defer f.rows.flush(false)

	tried := make(map[string]struct{})
	skipSuppliers := make(map[string]struct{})
	preferSupplier := ""

	s.stateMu.RLock()
	f.generation = s.health.Generation()
	f.aff = s.affinity.Lookup(resolveName, r.URL.Path, r.Header, body)
	if f.aff.Found && s.debugEnabled() {
		log.Printf("affinity hit rule=%s key=%s", f.aff.RuleName, f.aff.KeyID)
	}
	// Resolve the sticky (preferred) key once. Skip-retry only applies when the
	// pinned key is actually usable: after a reload it may have vanished, and
	// the request then falls back to normal selection with a full retry budget.
	var preferredKey registry.UpstreamKey
	var preferredLease pool.Lease
	preferredUsable := false
	if f.aff.Found {
		if _, keys, ok := s.reg.Resolve(resolveName); ok {
			if k, ok := affinity.ResolvePreferred(keys, f.aff.KeyID, tried); ok {
				if lease, admitted := s.selector.Acquire(resolveName, k); admitted {
					preferredKey, preferredLease, preferredUsable = k, lease, true
				}
			}
		}
	}
	maxAttempts := s.selector.MaxAttempts(resolveName)
	s.stateMu.RUnlock()
	if f.aff.Found && !preferredUsable {
		s.affinity.MarkPreferredUnavailable()
	}
	if preferredUsable && f.aff.SkipRetry {
		maxAttempts = 1
	}

	// Keepalive spans both halves of a streamed request: the wait for the
	// upstream's first event (the executor blocks on it before returning) and
	// every later gap between events. Committing the 200 from here is a
	// deliberate trade — the client gets bytes instead of silence, and a failure
	// that shows up afterwards is reported in-band because the status can no
	// longer change. Only requests that asked for stream: true are affected.
	if gjson.GetBytes(body, "stream").Bool() {
		f.keepalive = s.newStreamKeepalive(protocol)
	}
	if _, ok := w.(http.Flusher); !ok {
		f.keepalive = nil
	}
	defer f.keepalive.Stop()

	suffix := thinking.ParseSuffix(model)
	var lastErr error
	var availabilityErr *pool.AvailabilityError
	lastErrLogged := false
	var lastKey registry.UpstreamKey
	for n := range maxAttempts {
		var key registry.UpstreamKey
		var upstreamModel string
		var lease pool.Lease
		if n == 0 && preferredUsable {
			key, lease = preferredKey, preferredLease
			upstreamModel = preferredKey.UpstreamModelOr(resolveName)
			preferSupplier = preferredKey.Name
		} else {
			var pickErr error
			s.stateMu.RLock()
			key, upstreamModel, lease, pickErr = s.selector.PickWithLease(resolveName, tried, preferSupplier, skipSuppliers)
			s.stateMu.RUnlock()
			if pickErr != nil {
				lastErr = pickErr
				errors.As(pickErr, &availabilityErr)
				lastErrLogged = false
				break
			}
			if preferSupplier == "" {
				preferSupplier = key.Name
			}
		}
		tried[key.ID] = struct{}{}
		f.rows.flush(true)
		lastKey = key
		if suffix.HasSuffix {
			upstreamModel = thinking.ParseSuffix(upstreamModel).ModelName + "(" + suffix.RawSuffix + ")"
		}

		attemptCtx, cancel := context.WithCancel(r.Context())
		at := &upstreamAttempt{n: n, key: key, lease: lease, start: time.Now(), cancel: cancel}
		defer func() {
			cancel()
			s.selector.Release(at.lease)
		}()

		result, err := f.run(attemptCtx, execute, key, upstreamModel)
		if err != nil {
			cancel()
			if errors.Is(err, errStreamWriteFailed) {
				f.canceled(at, err.Error())
				return
			}
			if ctxErr := r.Context().Err(); ctxErr != nil {
				f.canceled(at, ctxErr.Error())
				return
			}
			lastErr = err
			lastErrLogged = false
			status, classStatus, retryAfter, msg := http.StatusBadGateway, 0, time.Duration(0), err.Error()
			if se, ok := err.(executor.StatusError); ok {
				if !retriableStatus(se.Code) {
					f.observe(at, se.Code, pool.OutcomeRequestError, time.Time{}, tokenUsage{}, 0)
					errBody := f.writeUpstreamError(se)
					f.rows.emit(f.record(key, se.Code, reqlog.OutcomeError, f.start, se.Error(), errBody))
					return
				}
				status, classStatus, retryAfter, msg = se.Code, se.Code, se.RetryAfter, se.Error()
			}
			f.clearAffinity(key.ID)
			f.observe(at, status, classifyAttempt(err, classStatus), time.Time{}, tokenUsage{}, retryAfter)
			f.rows.hold(f.record(key, status, reqlog.OutcomeError, at.start, msg, nil))
			lastErrLogged = true
			if s.debugEnabled() {
				log.Printf("upstream %s/%s failed status=%d, rotating (mode=%s)", key.Name, key.ID, status, key.FailoverMode)
			}
			if f.aff.Found && f.aff.SkipRetry && key.ID == f.aff.KeyID {
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
			cancel()
			f.canceled(at, ctxErr.Error())
			return
		}

		switch v := result.(type) {
		case *executor.Result:
			f.serveResult(at, v)
			return
		case *executor.StreamResult:
			f.serveStream(at, v)
			return
		default:
			cancel()
			f.observe(at, http.StatusBadGateway, pool.OutcomeUpstream, time.Time{}, tokenUsage{}, 0)
			lastErr = fmt.Errorf("unexpected executor result type %T", result)
			lastErrLogged = false
		}
	}
	f.finishFailed(lastKey, lastErr, lastErrLogged, availabilityErr)
}

// retriableStatus marks upstream statuses that fail over to another key.
func retriableStatus(code int) bool {
	return code == http.StatusUnauthorized || code == http.StatusForbidden ||
		code == http.StatusTooManyRequests || code >= http.StatusInternalServerError
}

// run executes one attempt. The executor blocks until the upstream's first SSE
// event, so when keepalive is armed the call runs in its own goroutine and is
// raced against the frame timer; a cancel closes the wait instead of holding
// the lease until the upstream decides to answer. Without keepalive (every
// non-stream request, and streams with it disabled) the attempt stays on this
// goroutine so a panic still unwinds through withRecover.
func (f *forwardRequest) run(
	ctx context.Context,
	execute func(ctx context.Context, key registry.UpstreamKey, upstreamModel string) (any, error),
	key registry.UpstreamKey, upstreamModel string,
) (any, error) {
	if f.keepalive == nil {
		return execute(ctx, key, upstreamModel)
	}
	done := make(chan attemptOutcome, 1)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				done <- attemptOutcome{err: fmt.Errorf("executor panic: %v", rec)}
			}
		}()
		res, err := execute(ctx, key, upstreamModel)
		done <- attemptOutcome{result: res, err: err}
	}()
	for {
		select {
		case out := <-done:
			return out.result, out.err
		case <-f.keepalive.C():
			f.commitStream()
			if _, err := f.w.Write(f.keepalive.frame); err != nil {
				return nil, fmt.Errorf("%w: %v", errStreamWriteFailed, err)
			}
			if fl, ok := f.w.(http.Flusher); ok {
				fl.Flush()
			}
			f.keepalive.Reset()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (f *forwardRequest) serveResult(at *upstreamAttempt, v *executor.Result) {
	f.w.Header().Set("Content-Type", "application/json")
	f.w.WriteHeader(http.StatusOK)
	_, writeErr := f.w.Write(v.Body)
	usage := usageFromResponse(v.Body)
	outcome, errMsg := reqlog.OutcomeCompleted, ""
	if writeErr != nil {
		outcome, errMsg = reqlog.OutcomeClientCanceled, writeErr.Error()
	} else if ctxErr := f.r.Context().Err(); ctxErr != nil {
		outcome, errMsg = reqlog.OutcomeClientCanceled, ctxErr.Error()
	}
	if outcome == reqlog.OutcomeCompleted {
		f.observe(at, v.Status, pool.OutcomeSuccess, time.Time{}, usage, 0)
		f.recordAffinity(at.key)
	} else {
		f.observe(at, v.Status, pool.OutcomeCanceled, time.Time{}, tokenUsage{}, 0)
	}
	at.cancel()
	f.rows.emit(f.record(at.key, http.StatusOK, outcome, f.start, errMsg, v.Body, usage))
}

func (f *forwardRequest) serveStream(at *upstreamAttempt, v *executor.StreamResult) {
	flusher, ok := f.w.(http.Flusher)
	if !ok {
		at.cancel()
		f.observe(at, http.StatusInternalServerError, pool.OutcomeRequestError, time.Time{}, tokenUsage{}, 0)
		writeAPIError(f.w, http.StatusInternalServerError, "server_error", "streaming not supported")
		f.rows.emit(f.record(at.key, http.StatusInternalServerError, reqlog.OutcomeError, f.start, "streaming not supported", nil))
		return
	}
	f.commitStream()
	flusher.Flush()

	ctx := f.r.Context()
	var streamErr string
	clientCanceled := false
	streamAborted := false
	var firstByteAt time.Time
	var usage tokenUsage
	abort := func(msg string, byClient bool) {
		streamErr = msg
		clientCanceled = clientCanceled || byClient
		streamAborted = true
		at.cancel()
	}
pump:
	for {
		select {
		case <-ctx.Done():
			abort(ctx.Err().Error(), true)
			break pump
		case <-f.keepalive.C():
			// Synthetic frame: the upstream has been silent past the idle
			// interval. It deliberately does not count as firstByteAt — no
			// upstream byte arrived.
			if _, err := f.w.Write(f.keepalive.frame); err != nil {
				abort(err.Error(), true)
				break pump
			}
			flusher.Flush()
			f.keepalive.Reset()
		case chunk, ok := <-v.Chunks:
			if !ok {
				break pump
			}
			if chunk.Err != nil {
				if f.s.debugEnabled() {
					log.Printf("stream error: %v", chunk.Err)
				}
				abort(chunk.Err.Error(), false)
				break pump
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
			if _, err := f.w.Write(chunk.Payload); err != nil {
				abort(err.Error(), true)
				break pump
			}
			flusher.Flush()
			f.keepalive.observe(chunk.Payload)
			f.keepalive.Reset()
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		clientCanceled = true
		streamErr = ctxErr.Error()
	}
	completion := executor.StreamFailed
	if !streamAborted && !clientCanceled {
		if v.Complete != nil {
			select {
			case completion = <-v.Complete:
			case <-ctx.Done():
				clientCanceled = true
				streamErr = ctx.Err().Error()
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
	at.cancel()

	outcome := reqlog.OutcomeCompleted
	if clientCanceled {
		outcome = reqlog.OutcomeClientCanceled
	} else if streamErr != "" {
		outcome = reqlog.OutcomeError
	}
	switch {
	case outcome == reqlog.OutcomeCompleted && completion == executor.StreamCompleted:
		f.observe(at, v.Status, pool.OutcomeSuccess, firstByteAt, usage, 0)
		f.recordAffinity(at.key)
	case clientCanceled:
		f.observe(at, v.Status, pool.OutcomeCanceled, firstByteAt, usage, 0)
	default:
		health := pool.OutcomeUpstream
		if completion == executor.StreamIncomplete {
			health = pool.OutcomeRequestError
		}
		f.clearAffinity(at.key.ID)
		f.observe(at, v.Status, health, firstByteAt, usage, 0)
	}
	f.rows.emit(f.record(at.key, http.StatusOK, outcome, f.start, streamErr, nil, usage))
}

// finishFailed answers a request whose attempts all failed or that ran out of
// candidates.
func (f *forwardRequest) finishFailed(lastKey registry.UpstreamKey, lastErr error, logged bool, availabilityErr *pool.AvailabilityError) {
	if availabilityErr != nil {
		seconds := int64(math.Ceil(time.Until(availabilityErr.RetryAt).Seconds()))
		if seconds < 1 {
			seconds = 1
		}
		f.w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
		msg := fmt.Sprintf("all upstream credentials are cooling down for model %s", availabilityErr.Model)
		f.writeError(http.StatusTooManyRequests, "rate_limit_error", msg)
		f.rows.emit(f.record(lastKey, http.StatusTooManyRequests, reqlog.OutcomeError, f.start, msg, nil))
		return
	}
	if se, ok := lastErr.(executor.StatusError); ok {
		errBody := f.writeUpstreamError(se)
		if !logged {
			f.rows.emit(f.record(lastKey, se.Code, reqlog.OutcomeError, f.start, se.Error(), errBody))
		}
		return
	}
	code, msg := http.StatusBadGateway, "all upstream credentials failed"
	if lastErr != nil {
		msg = lastErr.Error()
		if strings.Contains(msg, "model not found") {
			code = http.StatusNotFound
		}
	}
	f.writeError(code, "server_error", msg)
	if !logged {
		f.rows.emit(f.record(lastKey, code, reqlog.OutcomeError, f.start, msg, nil))
	}
}

func (f *forwardRequest) commitStream() {
	if f.committed {
		return
	}
	f.committed = true
	f.w.Header().Set("Content-Type", "text/event-stream")
	f.w.Header().Set("Cache-Control", "no-cache")
	f.w.Header().Set("Connection", "keep-alive")
	f.w.WriteHeader(http.StatusOK)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
}

// writeError reports a gateway-side failure; once the stream is committed the
// status can no longer change, so it goes in-band.
func (f *forwardRequest) writeError(status int, typ, msg string) {
	if f.committed {
		writeSSEError(f.w, f.protocol, msg)
		return
	}
	writeAPIError(f.w, status, typ, msg)
}

// writeUpstreamError relays an upstream error response and returns the body
// that was (or would have been) sent, for the request log.
func (f *forwardRequest) writeUpstreamError(se executor.StatusError) []byte {
	errBody := normalizeErrorBody(se.Body)
	if f.committed {
		writeSSEError(f.w, f.protocol, streamErrorMessage(errBody, se.Error()))
		return errBody
	}
	f.w.Header().Set("Content-Type", "application/json")
	f.w.WriteHeader(se.Code)
	_, _ = f.w.Write(errBody)
	return errBody
}

// recordAffinity and clearAffinity drop their update when a reload happened
// mid-request: the pin would point into a key pool that no longer exists.
func (f *forwardRequest) recordAffinity(key registry.UpstreamKey) {
	s := f.s
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	if f.generation != s.health.Generation() || !f.aff.Matched || f.aff.CacheKey == "" {
		return
	}
	if s.currentCfg().ChannelAffinity.SwitchOnSuccessOrDefault() || !f.aff.Found || f.aff.KeyID == key.ID {
		s.affinity.Record(f.aff.CacheKey, key.ID, f.aff.TTL)
		if s.debugEnabled() {
			log.Printf("affinity recorded rule=%s key=%s", f.aff.RuleName, key.ID)
		}
	}
}

func (f *forwardRequest) clearAffinity(keyID string) {
	s := f.s
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	if f.generation != s.health.Generation() || !f.aff.Matched || f.aff.CacheKey == "" || keyID != f.aff.KeyID {
		return
	}
	s.affinity.Clear(f.aff.CacheKey)
	if s.debugEnabled() {
		log.Printf("affinity cleared rule=%s key=%s", f.aff.RuleName, keyID)
	}
}

// canceled settles an attempt the client walked away from.
func (f *forwardRequest) canceled(at *upstreamAttempt, msg string) {
	at.cancel()
	f.observe(at, 0, pool.OutcomeCanceled, time.Time{}, tokenUsage{}, 0)
	f.rows.emit(f.record(at.key, 0, reqlog.OutcomeClientCanceled, at.start, msg, nil))
}

func (f *forwardRequest) observe(at *upstreamAttempt, status int, outcome pool.AttemptOutcome, firstByteAt time.Time, usage tokenUsage, retryAfter time.Duration) {
	f.s.observeAttemptWithTokens(f.reqID, at.n, f.resolveName, at.key, at.lease, status, outcome, at.start, firstByteAt, usage.outputTokens, usage.outputSeen, retryAfter)
}

func (f *forwardRequest) record(key registry.UpstreamKey, status int, outcome string, start time.Time, errMsg string, respBody []byte, usage ...tokenUsage) (reqlog.Record, bool) {
	return f.s.newLogRecord(f.reqID, f.r, f.protocol, f.model, key.Provider, key.Name, status, outcome, start, errMsg, f.body, respBody, usage...)
}
