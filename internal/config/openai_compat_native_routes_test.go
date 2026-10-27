package config

import "testing"

func TestOpenAICompatibilityNativeRouteConfig(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(`openai-compatibility:
  - name: native
    base-url: https://example.test/v1
    routes:
      embeddings: /custom/embed
    models:
      - alias: embed-alias
        name: embed-real
        capability: embedding
      - alias: rerank-alias
        name: rerank-real
        type: rerank
`))
	if err != nil {
		t.Fatal(err)
	}
	compat := cfg.OpenAICompatibility[0]
	if compat.Routes["embeddings"] != "/custom/embed" {
		t.Fatalf("route = %q", compat.Routes["embeddings"])
	}
	if compat.Models[0].Capability != "embedding" || compat.Models[1].Type != "rerank" {
		t.Fatalf("capabilities not decoded: %#v", compat.Models)
	}
}
