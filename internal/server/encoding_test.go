package server

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/Mieluoxxx/lite-cpa/internal/config"
)

func TestAcceptsGzip(t *testing.T) {
	cases := []struct {
		header string
		want   bool
	}{
		{"", false},
		{"identity", false},
		{"gzip", true},
		{"deflate, gzip, br, zstd", true},
		{"GZIP", true},
		{"x-gzip", true},
		{"gzip;q=0", false},
		{"gzip;q=0.5", true},
		{"gzip;q=0, *", false},
		{"gzip;q=0, *;q=1", false},
		{"*", true},
		{"*;q=0", false},
		{"br, zstd", false},
	}
	for _, tc := range cases {
		if got := acceptsGzip(tc.header); got != tc.want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}

func TestShouldCompress(t *testing.T) {
	cases := []struct {
		name   string
		code   int
		header map[string]string
		want   bool
	}{
		{"json", 200, map[string]string{"Content-Type": "application/json"}, true},
		{"json charset", 200, map[string]string{"Content-Type": "application/json; charset=utf-8"}, true},
		{"event stream", 200, map[string]string{"Content-Type": "text/event-stream"}, true},
		{"dashboard js", 200, map[string]string{"Content-Type": "text/javascript; charset=utf-8"}, true},
		{"no content type", 200, map[string]string{}, false},
		{"image", 200, map[string]string{"Content-Type": "image/png"}, false},
		{"already encoded", 200, map[string]string{"Content-Type": "application/json", "Content-Encoding": "br"}, false},
		{"partial content", 206, map[string]string{"Content-Type": "application/json"}, false},
		{"range response", 200, map[string]string{"Content-Type": "application/json", "Content-Range": "bytes 0-1/9"}, false},
		{"not modified", 304, map[string]string{"Content-Type": "application/json"}, false},
		{"tiny declared body", 200, map[string]string{"Content-Type": "application/json", "Content-Length": "20"}, false},
		{"large declared body", 200, map[string]string{"Content-Type": "application/json", "Content-Length": "4096"}, true},
	}
	for _, tc := range cases {
		h := http.Header{}
		for k, v := range tc.header {
			h.Set(k, v)
		}
		if got := shouldCompress(tc.code, h); got != tc.want {
			t.Errorf("%s: shouldCompress = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// gunzipPrefix decodes as much of a (possibly unfinished) gzip stream as has
// already reached the client.
func gunzipPrefix(t *testing.T, data []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("no gzip block flushed yet: %v", err)
	}
	defer zr.Close()
	out, _ := io.ReadAll(zr)
	return string(out)
}

func TestCompressJSONRoundTrip(t *testing.T) {
	payload := `{"hello":"world"}`
	s := New(&config.Config{}, nil)
	handler := s.compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Fatalf("Vary = %q, want Accept-Encoding", got)
	}
	zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("body = %q, want %q", got, payload)
	}
}

// A client that sends no Accept-Encoding must get a byte-identical response.
func TestCompressOffWithoutAcceptEncoding(t *testing.T) {
	payload := `{"hello":"world"}`
	s := New(&config.Config{}, nil)
	handler := s.compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty", got)
	}
	if got := rec.Body.String(); got != payload {
		t.Fatalf("body = %q, want %q", got, payload)
	}
}

func TestCompressDisabledByConfig(t *testing.T) {
	disabled := false
	s := New(&config.Config{Compression: &disabled}, nil)
	handler := s.compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hello":"world"}`))
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty when compression is disabled", got)
	}
}

// Compressed event streams must stay incremental: each event has to reach the
// client before the handler writes the next one.
func TestCompressStreamFlushesEachEvent(t *testing.T) {
	s := New(&config.Config{}, nil)
	var recorder *httptest.ResponseRecorder
	handler := s.compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("compressed writer does not implement http.Flusher")
		}
		_, _ = w.Write([]byte("data: one\n\n"))
		flusher.Flush()

		first := gunzipPrefix(t, recorder.Body.Bytes())
		if !strings.Contains(first, "data: one") {
			t.Fatalf("first event not flushed: %q", first)
		}
		if strings.Contains(first, "data: two") {
			t.Fatalf("second event leaked before it was written: %q", first)
		}

		_, _ = w.Write([]byte("data: two\n\n"))
		flusher.Flush()
	}))

	recorder = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	handler.ServeHTTP(recorder, req)

	if got := recorder.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := gunzipPrefix(t, recorder.Body.Bytes()); !strings.Contains(got, "data: one") || !strings.Contains(got, "data: two") {
		t.Fatalf("stream body = %q", got)
	}
}

// The middleware chain built by New must actually include compression.
func TestCompressionWiredIntoServer(t *testing.T) {
	s := New(&config.Config{APIKeys: []string{"sk-test"}}, nil)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("healthz Content-Encoding = %q, want gzip", got)
	}
	if got := gunzipPrefix(t, rec.Body.Bytes()); got != `{"status":"ok"}` {
		t.Fatalf("healthz body = %q", got)
	}
}

func TestDecodeBodyUnwrapsCompressedBodies(t *testing.T) {
	payload := `{"model":"m","stream":true}`

	var gzipped bytes.Buffer
	zw := gzip.NewWriter(&gzipped)
	_, _ = zw.Write([]byte(payload))
	_ = zw.Close()

	var deflated bytes.Buffer
	fl := zlib.NewWriter(&deflated)
	_, _ = fl.Write([]byte(payload))
	_ = fl.Close()

	var zstdCompressed bytes.Buffer
	zenc, err := zstd.NewWriter(&zstdCompressed)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	_, _ = zenc.Write([]byte(payload))
	_ = zenc.Close()

	for name, body := range map[string][]byte{
		"gzip":    gzipped.Bytes(),
		"deflate": deflated.Bytes(),
		"zstd":    zstdCompressed.Bytes(),
	} {
		t.Run(name, func(t *testing.T) {
			s := New(&config.Config{MaxBodyBytes: 1 << 20}, nil)
			handler := s.decodeBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Content-Encoding"); got != "" {
					t.Errorf("Content-Encoding still set: %q", got)
				}
				if r.ContentLength != -1 {
					t.Errorf("ContentLength = %d, want -1", r.ContentLength)
				}
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("read body: %v", err)
				}
				if string(raw) != payload {
					t.Fatalf("body = %q, want %q", raw, payload)
				}
			}))

			req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			req.Header.Set("Content-Encoding", name)
			req.Header.Set("Content-Type", "application/json")
			handler.ServeHTTP(httptest.NewRecorder(), req)
		})
	}
}

func TestDecodeBodyRejectsUnsupportedEncoding(t *testing.T) {
	s := New(&config.Config{MaxBodyBytes: 1 << 20}, nil)
	handler := s.decodeBody(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler must not run for an unsupported Content-Encoding")
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("x"))
	req.Header.Set("Content-Encoding", "br")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnsupportedMediaType)
	}
}

// A small compressed body must not expand past the body cap.
func TestDecodeBodyCapsDecompressedSize(t *testing.T) {
	fixtures := map[string]func(*testing.T) []byte{}
	fixtures["gzip"] = func(t *testing.T) []byte {
		t.Helper()
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(bytes.Repeat([]byte("a"), 8192))
		_ = zw.Close()
		return buf.Bytes()
	}
	fixtures["zstd"] = func(t *testing.T) []byte {
		t.Helper()
		var buf bytes.Buffer
		zw, err := zstd.NewWriter(&buf)
		if err != nil {
			t.Fatalf("zstd writer: %v", err)
		}
		_, _ = zw.Write(bytes.Repeat([]byte("a"), 8192))
		_ = zw.Close()
		return buf.Bytes()
	}

	for name, build := range fixtures {
		t.Run(name, func(t *testing.T) {
			body := build(t)
			if len(body) >= 1024 {
				t.Fatalf("fixture should be a compression bomb, got %d bytes", len(body))
			}

			s := New(&config.Config{MaxBodyBytes: 64}, nil)
			handler := s.decodeBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := io.ReadAll(r.Body); err == nil {
					t.Fatal("expected the decompressed body to exceed the cap")
				}
				w.WriteHeader(http.StatusRequestEntityTooLarge)
			}))

			req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			req.Header.Set("Content-Encoding", name)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413", rec.Code)
			}
		})
	}
}

func TestDecodeBodyLeavesMultipartAlone(t *testing.T) {
	raw := []byte("--b\r\nContent-Disposition: form-data; name=\"x\"\r\n\r\n1\r\n--b--\r\n")
	s := New(&config.Config{MaxBodyBytes: 1 << 20}, nil)
	handler := s.decodeBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if !bytes.Equal(got, raw) {
			t.Fatalf("multipart body was rewritten: %q", got)
		}
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=b")
	handler.ServeHTTP(httptest.NewRecorder(), req)
}
