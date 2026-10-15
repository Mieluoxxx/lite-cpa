package server

import (
	"testing"
	"time"

	"github.com/Mieluoxxx/lite-cpa/internal/config"
)

func TestStreamKeepaliveFramesPerProtocol(t *testing.T) {
	s := New(&config.Config{}, nil)
	cases := map[string]string{
		"responses": "data: {\"type\":\"response.in_progress\"}\n\n",
		"chat":      ": keepalive\n\n",
		"claude":    "event: ping\ndata: {\"type\":\"ping\"}\n\n",
	}
	for protocol, want := range cases {
		k := s.newStreamKeepalive(protocol)
		if k == nil {
			t.Fatalf("%s: keepalive is on by default and must have a frame", protocol)
		}
		if string(k.frame) != want {
			t.Errorf("%s: frame = %q, want %q", protocol, k.frame, want)
		}
		k.Stop()
	}
	if k := s.newStreamKeepalive("openai-image"); k != nil {
		t.Error("image responses must not carry SSE keepalive frames")
	}

	disabled := false
	off := New(&config.Config{SSEKeepalive: config.SSEKeepaliveConfig{Enabled: &disabled}}, nil)
	if k := off.newStreamKeepalive("responses"); k != nil {
		t.Error("sse-keepalive: false must disable frames")
	}
}

func TestStreamKeepaliveTimerLifecycle(t *testing.T) {
	s := New(&config.Config{SSEKeepalive: config.SSEKeepaliveConfig{
		FirstFrameWait: "20ms",
		Interval:       "30ms",
	}}, nil)
	k := s.newStreamKeepalive("responses")
	defer k.Stop()

	select {
	case <-k.C():
	case <-time.After(2 * time.Second):
		t.Fatal("first keepalive frame never fired")
	}

	// Real upstream data restarts the idle countdown.
	k.Reset()
	select {
	case <-k.C():
		t.Fatal("keepalive fired immediately after real data")
	case <-time.After(10 * time.Millisecond):
	}
	select {
	case <-k.C():
	case <-time.After(2 * time.Second):
		t.Fatal("keepalive did not resume after the idle interval")
	}

	// A terminal event ends keepalive for good.
	k.observe([]byte(`data: {"type":"response.completed"}`))
	if k.C() != nil {
		t.Fatal("keepalive must stop after a terminal event")
	}
}

func TestStreamTerminated(t *testing.T) {
	cases := []struct {
		payload string
		want    bool
	}{
		{`data: {"type":"response.output_text.delta","delta":"hi"}`, false},
		{`data: {"type":"response.completed"}`, true},
		{`data: {"type":"response.failed"}`, true},
		{`data: {"type":"response.incomplete"}`, true},
		{`event: message_stop\ndata: {"type":"message_stop"}`, true},
		{"data: [DONE]", true},
		{`data: {"type":"ping"}`, false},
	}
	for _, tc := range cases {
		if got := streamTerminated([]byte(tc.payload)); got != tc.want {
			t.Errorf("streamTerminated(%q) = %v, want %v", tc.payload, got, tc.want)
		}
	}
}

// A disabled (nil) keepalive must be inert everywhere it is used from forward().
func TestNilStreamKeepaliveIsSafe(t *testing.T) {
	var k *streamKeepalive
	if k.C() != nil {
		t.Error("nil keepalive must expose a nil select arm")
	}
	k.Reset()
	k.observe([]byte(`data: {"type":"response.completed"}`))
	k.Stop()
}
