package config

import "testing"

func TestOpenAICompatibilityNativeRouteConfig(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(`openai-compatibility:
  - name: native
    base-url: https://example.test/v1
    routes:
      embeddings: /custom/embed
      image: /custom/images/generations
      video_submit: /custom/video/submit
      video_status: /custom/video/status
    models:
      - alias: embed-alias
        name: embed-real
        capability: embedding
      - alias: rerank-alias
        name: rerank-real
        type: rerank
      - alias: image-alias
        name: image-real
        capability: image
      - alias: video-alias
        name: video-real
        capability: video
`))
	if err != nil {
		t.Fatal(err)
	}
	compat := cfg.OpenAICompatibility[0]
	if compat.Routes["embeddings"] != "/custom/embed" {
		t.Fatalf("route = %q", compat.Routes["embeddings"])
	}
	if compat.Routes["image"] != "/custom/images/generations" {
		t.Fatalf("image route = %q", compat.Routes["image"])
	}
	if compat.Routes["video_submit"] != "/custom/video/submit" {
		t.Fatalf("video submit route = %q", compat.Routes["video_submit"])
	}
	if compat.Routes["video_status"] != "/custom/video/status" {
		t.Fatalf("video status route = %q", compat.Routes["video_status"])
	}
	if compat.Models[0].Capability != "embedding" || compat.Models[1].Type != "rerank" {
		t.Fatalf("capabilities not decoded: %#v", compat.Models)
	}
	if compat.Models[2].Capability != "image" || compat.Models[3].Capability != "video" {
		t.Fatalf("image/video capabilities not decoded: %#v", compat.Models)
	}
}
