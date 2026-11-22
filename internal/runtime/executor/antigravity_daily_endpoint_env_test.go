package executor

import (
	"testing"

	antigravityauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravity"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestResolveAntigravityRequestBaseURL_EnvRelayEnforcement(t *testing.T) {
	t.Run("default without env uses official daily endpoint", func(t *testing.T) {
		t.Setenv(antigravityauth.EnvDailyAPIEndpoint, "")
		auth := &cliproxyauth.Auth{ID: "a1"}
		if got := resolveAntigravityRequestBaseURL(auth); got != antigravityBaseURLDaily {
			t.Fatalf("expected %q, got %q", antigravityBaseURLDaily, got)
		}
		if got := antigravityLoadCodeAssistBaseURL(auth); got != antigravityBaseURLDaily {
			t.Fatalf("expected %q, got %q", antigravityBaseURLDaily, got)
		}
	})

	t.Run("env relay applies when auth has no base_url", func(t *testing.T) {
		t.Setenv(antigravityauth.EnvDailyAPIEndpoint, "http://127.0.0.1:28100/")
		auth := &cliproxyauth.Auth{ID: "a2"}
		if got := resolveAntigravityRequestBaseURL(auth); got != "http://127.0.0.1:28100" {
			t.Fatalf("expected env relay, got %q", got)
		}
		if got := antigravityLoadCodeAssistBaseURL(auth); got != "http://127.0.0.1:28100" {
			t.Fatalf("expected env relay for loadCodeAssist, got %q", got)
		}
	})

	t.Run("env relay overrides official cloudcode base_url to prevent direct bypass", func(t *testing.T) {
		t.Setenv(antigravityauth.EnvDailyAPIEndpoint, "http://127.0.0.1:28100")
		for _, raw := range []string{
			"https://daily-cloudcode-pa.googleapis.com",
			"https://daily-cloudcode-pa.googleapis.com/",
			"https://cloudcode-pa.googleapis.com",
			"https://daily-cloudcode-pa.sandbox.googleapis.com",
		} {
			auth := &cliproxyauth.Auth{
				ID:       "a3",
				Metadata: map[string]any{"base_url": raw},
			}
			if got := resolveAntigravityRequestBaseURL(auth); got != "http://127.0.0.1:28100" {
				t.Fatalf("raw=%q: expected forced relay, got %q", raw, got)
			}
		}
	})

	t.Run("explicit non-official custom relay is preserved", func(t *testing.T) {
		t.Setenv(antigravityauth.EnvDailyAPIEndpoint, "http://127.0.0.1:28100")
		auth := &cliproxyauth.Auth{
			ID:       "a4",
			Metadata: map[string]any{"base_url": "http://127.0.0.1:29999/"},
		}
		if got := resolveAntigravityRequestBaseURL(auth); got != "http://127.0.0.1:29999" {
			t.Fatalf("expected custom relay preserved, got %q", got)
		}
	})
}
