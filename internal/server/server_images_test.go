package server_test

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Mieluoxxx/lite-cpa/internal/config"
	"github.com/Mieluoxxx/lite-cpa/internal/server"
	"github.com/tidwall/gjson"
)

func imageCfg(t *testing.T, up *httptest.Server) *config.Config {
	t.Helper()
	return &config.Config{
		Host:         "127.0.0.1",
		Port:         freePort(t),
		APIKeys:      []string{"sk-test"},
		RequestRetry: 1,
		MaxBodyBytes: 1 << 20,
		OpenAIImages: []config.Provider{{
			Name:    "mock-image",
			BaseURL: up.URL,
			APIKey:  "sk-up",
			Models:  []config.ModelAlias{{Name: "gpt-image-2", Alias: "gpt-image-2"}},
		}},
	}
}

// TestImagesGenerationsPassthrough verifies that a JSON /v1/images/generations
// request reaches the upstream at /images/generations with body and auth intact,
// and that the upstream JSON response is forwarded verbatim to the client.
func TestImagesGenerationsPassthrough(t *testing.T) {
	var gotPath, gotAuth, gotCT, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1700000000,"data":[{"b64_json":"QkFGRQ=="}],"usage":{"input_tokens":12,"output_tokens":34,"total_tokens":46}}`))
	}))
	t.Cleanup(up.Close)

	cfg := imageCfg(t, up)
	srv := server.New(cfg, nil)
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	waitHTTP(t, "http://127.0.0.1:"+strconv.Itoa(cfg.Port)+"/healthz")

	req, _ := http.NewRequest(http.MethodPost,
		"http://127.0.0.1:"+strconv.Itoa(cfg.Port)+"/v1/images/generations",
		strings.NewReader(`{"model":"gpt-image-2","prompt":"a cat","size":"1024x1024"}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d body %s", resp.StatusCode, raw)
	}
	if gotPath != "/images/generations" {
		t.Fatalf("upstream path %q, want /images/generations", gotPath)
	}
	if gotAuth != "Bearer sk-up" {
		t.Fatalf("auth %q", gotAuth)
	}
	if gotCT != "application/json" {
		t.Fatalf("content-type %q", gotCT)
	}
	// Body forwarded verbatim (model field untouched — no translation).
	if gjson.Get(gotBody, "prompt").String() != "a cat" {
		t.Fatalf("upstream body %s", gotBody)
	}
	// Response forwarded verbatim.
	if gjson.GetBytes(raw, "data.0.b64_json").String() != "QkFGRQ==" {
		t.Fatalf("response %s", raw)
	}
}

// TestImagesEditsMultipartPassthrough verifies that a multipart /v1/images/edits
// upload is forwarded byte-for-byte (boundary intact) and is still parseable
// upstream-side, while model is correctly read from the form for routing.
func TestImagesEditsMultipartPassthrough(t *testing.T) {
	var gotPath, gotCT string
	var gotBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"QkFHRUQ="}]}`))
	}))
	t.Cleanup(up.Close)

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	_ = writer.WriteField("model", "gpt-image-2")
	_ = writer.WriteField("prompt", "edit this")
	_ = writer.WriteField("stream", "false")
	fw, _ := writer.CreateFormFile("image[]", "source.png")
	_, _ = fw.Write([]byte("\x89PNG\r\n\x1a\nFAKEIMAGEBYTES"))
	_ = writer.Close()
	contentType := writer.FormDataContentType()

	cfg := imageCfg(t, up)
	srv := server.New(cfg, nil)
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	waitHTTP(t, "http://127.0.0.1:"+strconv.Itoa(cfg.Port)+"/healthz")

	req, _ := http.NewRequest(http.MethodPost,
		"http://127.0.0.1:"+strconv.Itoa(cfg.Port)+"/v1/images/edits",
		bytes.NewReader(buf.Bytes()))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d body %s", resp.StatusCode, raw)
	}
	if gotPath != "/images/edits" {
		t.Fatalf("upstream path %q, want /images/edits", gotPath)
	}
	// Multipart Content-Type (with boundary) must travel intact.
	if !strings.HasPrefix(gotCT, "multipart/form-data; boundary=") {
		t.Fatalf("content-type %q, want multipart with boundary", gotCT)
	}
	// Upstream received the exact same multipart bytes (verbatim passthrough).
	if !bytes.Equal(gotBody, buf.Bytes()) {
		t.Fatalf("upstream body differs from sent body (up=%d sent=%d)", len(gotBody), buf.Len())
	}
	// And the multipart is still parseable upstream-side, model + image file intact.
	reader := multipart.NewReader(bytes.NewReader(gotBody), strings.TrimPrefix(gotCT, "multipart/form-data; boundary="))
	form, err := reader.ReadForm(32 << 20)
	if err != nil {
		t.Fatalf("reparse upstream multipart: %v", err)
	}
	defer form.RemoveAll()
	if len(form.Value["model"]) == 0 || form.Value["model"][0] != "gpt-image-2" {
		t.Fatalf("model field %v", form.Value["model"])
	}
	if len(form.File["image[]"]) != 1 {
		t.Fatalf("image files %d, want 1", len(form.File["image[]"]))
	}
}

// TestImagesStreamPassthrough verifies SSE image events are forwarded verbatim.
func TestImagesStreamPassthrough(t *testing.T) {
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: image_generation.partial_image\ndata: {\"type\":\"image_generation.partial_image\",\"partial_image_index\":0,\"b64_json\":\"AA==\"}\n\n"))
		_, _ = w.Write([]byte("event: image_generation.completed\ndata: {\"type\":\"image_generation.completed\",\"b64_json\":\"BB==\"}\n\n"))
	}))
	t.Cleanup(up.Close)

	cfg := imageCfg(t, up)
	srv := server.New(cfg, nil)
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	waitHTTP(t, "http://127.0.0.1:"+strconv.Itoa(cfg.Port)+"/healthz")

	req, _ := http.NewRequest(http.MethodPost,
		"http://127.0.0.1:"+strconv.Itoa(cfg.Port)+"/v1/images/generations",
		strings.NewReader(`{"model":"gpt-image-2","prompt":"city","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if gotPath != "/images/generations" {
		t.Fatalf("upstream path %q", gotPath)
	}
	out := string(raw)
	if !strings.Contains(out, "image_generation.partial_image") || !strings.Contains(out, "image_generation.completed") {
		t.Fatalf("stream output missing image events: %q", out)
	}
}

// TestImagesMissingModel verifies model-required validation before any upstream call.
func TestImagesMissingModel(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should not be called")
	}))
	t.Cleanup(up.Close)

	cfg := imageCfg(t, up)
	srv := server.New(cfg, nil)
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	waitHTTP(t, "http://127.0.0.1:"+strconv.Itoa(cfg.Port)+"/healthz")

	req, _ := http.NewRequest(http.MethodPost,
		"http://127.0.0.1:"+strconv.Itoa(cfg.Port)+"/v1/images/generations",
		strings.NewReader(`{"prompt":"no model"}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d, want 400; body %s", resp.StatusCode, body)
	}
}
