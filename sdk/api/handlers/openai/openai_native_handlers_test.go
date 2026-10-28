package openai

import (
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestNativeProviderModelAndRoute(t *testing.T) {
	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
		Name: "native",
		Routes: map[string]string{
			"embedding":    "/custom/embed",
			"image":        "/custom/image",
			"video_submit": "/custom/video/submit",
			"video_status": "/custom/video/status",
		},
		Models: []config.OpenAICompatibilityModel{
			{Alias: "e", Name: "real-e", Capability: "embedding"},
			{Name: "r", Type: "rerank"},
			{Alias: "img", Name: "real-img", Capability: "image"},
			{Alias: "vid", Name: "real-vid", Capability: "video"},
		},
	}}}
	provider, upstream, ok := nativeProviderModel(cfg, "e", "embedding")
	if !ok || provider != "native" || upstream != "real-e" {
		t.Fatalf("got %q %q %v", provider, upstream, ok)
	}
	if _, _, ok := nativeProviderModel(cfg, "e", "rerank"); ok {
		t.Fatal("capability mismatch accepted")
	}
	provider, upstream, ok = nativeProviderModel(cfg, "img", "image")
	if !ok || provider != "native" || upstream != "real-img" {
		t.Fatalf("got %q %q %v for image", provider, upstream, ok)
	}
	provider, upstream, ok = nativeProviderModel(cfg, "vid", "video")
	if !ok || provider != "native" || upstream != "real-vid" {
		t.Fatalf("got %q %q %v for video", provider, upstream, ok)
	}
	if got := nativeRoute(&cfg.OpenAICompatibility[0], "embedding"); got != "/custom/embed" {
		t.Fatalf("route %q", got)
	}
	if got := nativeRoute(&cfg.OpenAICompatibility[0], "image"); got != "/custom/image" {
		t.Fatalf("route %q", got)
	}
	if got := nativeRoute(&cfg.OpenAICompatibility[0], "video_submit"); got != "/custom/video/submit" {
		t.Fatalf("route %q", got)
	}
	if got := nativeRoute(&cfg.OpenAICompatibility[0], "video_status"); got != "/custom/video/status" {
		t.Fatalf("route %q", got)
	}
	if got := nativeRoute(&cfg.OpenAICompatibility[0], "asr"); got != "/audio/transcriptions" {
		t.Fatalf("default route %q", got)
	}
}

func TestRewriteMultipartModelPreservesFile(t *testing.T) {
	var b strings.Builder
	w := multipart.NewWriter(&b)
	_ = w.WriteField("model", "alias")
	part, err := w.CreateFormFile("file", "a.wav")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("audio"))
	_ = w.Close()
	got, err := rewriteMultipartModel([]byte(b.String()), w.FormDataContentType(), "real-model")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader(string(got)))
	r.Header.Set("Content-Type", w.FormDataContentType())
	if err := r.ParseMultipartForm(1024); err != nil {
		t.Fatal(err)
	}
	if r.FormValue("model") != "real-model" || string(r.MultipartForm.File["file"][0].Filename) != "a.wav" {
		t.Fatalf("multipart rewrite failed: %#v", r.MultipartForm)
	}
}
