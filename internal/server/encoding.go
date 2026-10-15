package server

import (
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// Response compression and request decompression.
//
// Outbound: responses are gzipped only when the client advertised gzip, so a
// client that sends no Accept-Encoding gets byte-identical responses. Streaming
// responses stay incremental because every handler Flush pushes the gzip block
// and then the connection.
//
// Inbound: a client may hand us a gzip/deflate-compressed JSON body. Handlers
// parse plain JSON, so the body is unwrapped before they see it.

// compressMinBytes skips compression for bodies whose size the handler already
// declared, where gzip framing would cost more than it saves.
const compressMinBytes = 1024

// compress wraps responses in gzip for clients that advertise it.
func (s *Server) compress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.compressEnabled() || !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			next.ServeHTTP(w, r)
			return
		}
		cw := &compressWriter{ResponseWriter: w}
		defer cw.Close()
		next.ServeHTTP(cw, r)
	})
}

type compressWriter struct {
	http.ResponseWriter
	gz      *gzip.Writer
	sticky  error
	decided bool
}

// WriteHeader decides once, before any body byte, whether this response is
// compressed. Event streams decide on the handler's explicit WriteHeader(200)
// call and start gzipping immediately; JSON is compressed unless it declared a
// size below the threshold.
func (w *compressWriter) WriteHeader(code int) {
	if !w.decided {
		w.decided = true
		if shouldCompress(code, w.Header()) {
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Add("Vary", "Accept-Encoding")
			w.Header().Del("Content-Length")
			w.gz = gzip.NewWriter(w.ResponseWriter)
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write returns the uncompressed byte count, as an io.Writer must. A write
// error that gzip only surfaces on Flush is remembered and returned by the next
// Write, so the streaming loop still notices a client that went away.
func (w *compressWriter) Write(b []byte) (int, error) {
	if !w.decided {
		w.WriteHeader(http.StatusOK)
	}
	if w.sticky != nil {
		return 0, w.sticky
	}
	if w.gz != nil {
		n, err := w.gz.Write(b)
		if err != nil {
			w.sticky = err
		}
		return n, err
	}
	n, err := w.ResponseWriter.Write(b)
	if err != nil {
		w.sticky = err
	}
	return n, err
}

// Flush pushes one complete gzip block and then flushes the connection. Without
// this method forward()'s http.Flusher assertion fails and every streaming
// request becomes "streaming not supported".
func (w *compressWriter) Flush() {
	if w.gz != nil {
		if err := w.gz.Flush(); err != nil && w.sticky == nil {
			w.sticky = err
		}
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Close terminates the gzip stream so the final block and trailer reach the
// client. Idempotent.
func (w *compressWriter) Close() {
	if w.gz != nil {
		_ = w.gz.Close()
		w.gz = nil
	}
}

func (s *Server) compressEnabled() bool {
	cfg := s.currentCfg()
	return cfg != nil && cfg.CompressionEnabled()
}

// shouldCompress reports whether a response with this status and header set is
// worth gzipping.
func shouldCompress(code int, h http.Header) bool {
	switch code {
	case http.StatusNoContent, http.StatusNotModified, http.StatusPartialContent:
		return false
	}
	// Never double-encode, and never touch a range response: the client asked
	// for specific bytes of the identity representation.
	if h.Get("Content-Encoding") != "" || h.Get("Content-Range") != "" {
		return false
	}
	if !compressibleType(h.Get("Content-Type")) {
		return false
	}
	if n, err := strconv.Atoi(h.Get("Content-Length")); err == nil && n < compressMinBytes {
		return false
	}
	return true
}

// compressibleType covers the JSON APIs and the dashboard text assets. Binary
// payloads (images, fonts) are already compressed or incompressible.
func compressibleType(contentType string) bool {
	if contentType == "" {
		return false
	}
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	if mt == "text/event-stream" || strings.HasPrefix(mt, "text/") || strings.HasSuffix(mt, "+json") {
		return true
	}
	switch mt {
	case "application/json", "application/javascript", "application/x-ndjson", "application/xml", "image/svg+xml":
		return true
	}
	return false
}

// acceptsGzip reports whether the client accepts gzip with a non-zero quality.
// An explicit gzip entry wins over a wildcard, so "gzip;q=0, *" means no.
func acceptsGzip(header string) bool {
	wildcard := false
	rejected := false
	for _, part := range strings.Split(header, ",") {
		fields := strings.Split(part, ";")
		encoding := strings.ToLower(strings.TrimSpace(fields[0]))
		q := 1.0
		for _, param := range fields[1:] {
			if value, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(param)), "q="); ok {
				if parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
					q = parsed
				}
			}
		}
		switch encoding {
		case "gzip", "x-gzip":
			if q > 0 {
				return true
			}
			rejected = true
		case "*":
			wildcard = q > 0
		}
	}
	return wildcard && !rejected
}

// zstdDecodeMaxMemory bounds what a zstd frame header can make the decoder
// allocate. A frame may declare a window far larger than the body it carries,
// and the decompressed-size cap below would only catch that after the fact.
const zstdDecodeMaxMemory = 64 << 20

// decodeBody unwraps a compressed client request body so downstream parsing
// (gjson affinity, translation, request log) sees plain JSON.
//
// It must run inside limitBody: the configured byte cap then applies to the
// bytes the client actually sent, and the decompressed stream gets a second cap
// of its own — an over-limit body fails the read rather than being truncated or
// expanded without bound.
func (s *Server) decodeBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encoding := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
		if encoding == "" || encoding == "identity" || r.Body == nil || r.ContentLength == 0 {
			next.ServeHTTP(w, r)
			return
		}
		// Multipart uploads carry their own boundary through; leave them alone.
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err == nil && strings.HasPrefix(mt, "multipart/") {
			next.ServeHTTP(w, r)
			return
		}

		var decoded io.ReadCloser
		switch encoding {
		case "gzip", "x-gzip":
			zr, err := gzip.NewReader(r.Body)
			if err != nil {
				writeAPIError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("decode gzip body: %v", err))
				return
			}
			decoded = zr
		case "deflate":
			zr, err := zlib.NewReader(r.Body)
			if err != nil {
				writeAPIError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("decode deflate body: %v", err))
				return
			}
			decoded = zr
		case "zstd":
			// Codex compresses large Responses payloads (the compacted
			// transcript) this way; one-shot decode, so one goroutine.
			zr, err := zstd.NewReader(r.Body,
				zstd.WithDecoderConcurrency(1),
				zstd.WithDecoderMaxMemory(zstdDecodeMaxMemory),
			)
			if err != nil {
				writeAPIError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("decode zstd body: %v", err))
				return
			}
			decoded = zr.IOReadCloser()
		default:
			writeAPIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", "unsupported Content-Encoding: "+encoding)
			return
		}
		defer decoded.Close()

		if max := s.maxBody.Load(); max > 0 {
			r.Body = http.MaxBytesReader(w, decoded, max)
		} else {
			r.Body = decoded
		}
		r.ContentLength = -1
		r.Header.Del("Content-Encoding")
		r.Header.Del("Content-Length")
		next.ServeHTTP(w, r)
	})
}
