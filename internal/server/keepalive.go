package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/tidwall/gjson"
)

// Client-visible SSE keepalive frames, one per client protocol. The payloads
// match ai-sse-keepalive-proxy so clients already tolerate them: Responses uses
// a real in-progress event, Chat Completions uses an SSE comment (invisible to
// any data parser), Anthropic Messages uses its own ping event.
var (
	keepaliveFrameResponses = []byte("data: {\"type\":\"response.in_progress\"}\n\n")
	keepaliveFrameChat      = []byte(": keepalive\n\n")
	keepaliveFrameClaude    = []byte("event: ping\ndata: {\"type\":\"ping\"}\n\n")
)

// streamKeepalive drives the idle frame timer for one streaming response: a
// first frame after a short wait, then one per idle interval, so an upstream
// that is silent (long reasoning, slow first token) does not leave the client
// staring at a response body with no bytes in it.
type streamKeepalive struct {
	frame []byte
	idle  time.Duration
	timer *time.Timer
	done  bool
}

// newStreamKeepalive returns nil when keepalive is disabled or the client
// protocol has no frame (images, unknown formats). A nil receiver is safe to
// use everywhere below.
func (s *Server) newStreamKeepalive(protocol string) *streamKeepalive {
	cfg := s.currentCfg()
	if cfg == nil || !cfg.SSEKeepalive.EnabledOrDefault() {
		return nil
	}
	var frame []byte
	switch protocol {
	case "responses":
		frame = keepaliveFrameResponses
	case "chat":
		frame = keepaliveFrameChat
	case "claude":
		frame = keepaliveFrameClaude
	default:
		return nil
	}
	first := cfg.SSEKeepalive.FirstFrameWaitDuration()
	return &streamKeepalive{frame: frame, idle: cfg.SSEKeepalive.IntervalDuration(), timer: time.NewTimer(first)}
}

// C is the select arm. It is nil once no further synthetic frame may be sent,
// which blocks that arm forever.
func (k *streamKeepalive) C() <-chan time.Time {
	if k == nil || k.done {
		return nil
	}
	return k.timer.C
}

// Reset restarts the idle countdown after real upstream data reached the client.
func (k *streamKeepalive) Reset() {
	if k == nil || k.done {
		return
	}
	if !k.timer.Stop() {
		select {
		case <-k.timer.C:
		default:
		}
	}
	k.timer.Reset(k.idle)
}

// Stop ends keepalive for this response. Idempotent, nil-safe.
func (k *streamKeepalive) Stop() {
	if k == nil || k.done {
		return
	}
	k.done = true
	if !k.timer.Stop() {
		select {
		case <-k.timer.C:
		default:
		}
	}
}

// observe disables keepalive once the payload ends the client-visible stream,
// so no synthetic frame follows a terminal event.
func (k *streamKeepalive) observe(payload []byte) {
	if k != nil && streamTerminated(payload) {
		k.Stop()
	}
}

// streamErrorFrame is the in-band failure report for a client whose event
// stream is already committed, where no HTTP status can describe the failure
// any more.
func streamErrorFrame(protocol, message string) []byte {
	quoted, err := json.Marshal(message)
	if err != nil {
		quoted = []byte(`"upstream stream failed"`)
	}
	switch protocol {
	case "responses":
		return []byte("event: error\ndata: {\"type\":\"error\",\"code\":null,\"param\":null,\"message\":" + string(quoted) + "}\n\n")
	case "chat":
		return []byte("data: {\"error\":{\"type\":\"stream_error\",\"message\":" + string(quoted) + "}}\n\ndata: [DONE]\n\n")
	case "claude":
		return []byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":" + string(quoted) + "}}\n\n")
	}
	return nil
}

// writeSSEError reports a failure in-band on an already committed stream.
func writeSSEError(w http.ResponseWriter, protocol, message string) {
	frame := streamErrorFrame(protocol, message)
	if frame == nil {
		return
	}
	if _, err := w.Write(frame); err != nil {
		return
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// streamErrorMessage is the client-facing text of an upstream failure: the
// upstream's own message when its body carries one, else the status summary.
func streamErrorMessage(errBody []byte, fallback string) string {
	if msg := gjson.GetBytes(errBody, "error.message").String(); msg != "" {
		return msg
	}
	return fallback
}

// errStreamWriteFailed marks a keepalive write that failed because the client is
// gone; the attempt must stop instead of failing over to another key.
var errStreamWriteFailed = errors.New("stream keepalive write failed")

// attemptOutcome is one executor call's result, delivered by the goroutine that
// races it against the keepalive timer.
type attemptOutcome struct {
	result any
	err    error
}

// streamTerminated reports whether an event payload ends the response. Chunks
// arrive as whole events, so a substring check is enough; a false negative only
// costs one extra keepalive frame after completion.
func streamTerminated(payload []byte) bool {
	for _, marker := range [][]byte{
		[]byte(`"response.completed"`),
		[]byte(`"response.failed"`),
		[]byte(`"response.incomplete"`),
		[]byte(`"message_stop"`),
		[]byte("[DONE]"),
	} {
		if bytes.Contains(payload, marker) {
			return true
		}
	}
	return false
}
