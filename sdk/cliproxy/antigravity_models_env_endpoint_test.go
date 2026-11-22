package cliproxy

import (
	"reflect"
	"testing"

	antigravityauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravity"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestAntigravityModelBaseURLs_EnvRelayEnforcement(t *testing.T) {
	t.Run("default without env returns official daily endpoint", func(t *testing.T) {
		t.Setenv(antigravityauth.EnvDailyAPIEndpoint, "")
		got := antigravityModelBaseURLs(&coreauth.Auth{ID: "m1"})
		want := []string{antigravityModelBaseURLDaily}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("expected %v, got %v", want, got)
		}
	})

	t.Run("env relay applies when auth has no base_url", func(t *testing.T) {
		t.Setenv(antigravityauth.EnvDailyAPIEndpoint, "http://127.0.0.1:28100/")
		got := antigravityModelBaseURLs(&coreauth.Auth{ID: "m2"})
		want := []string{"http://127.0.0.1:28100"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("expected %v, got %v", want, got)
		}
	})

	t.Run("env relay overrides official endpoints in base_url and base_urls", func(t *testing.T) {
		t.Setenv(antigravityauth.EnvDailyAPIEndpoint, "http://127.0.0.1:28100")
		authSingle := &coreauth.Auth{
			ID:       "m3",
			Metadata: map[string]any{"base_url": "https://daily-cloudcode-pa.googleapis.com"},
		}
		if got := antigravityModelBaseURLs(authSingle); !reflect.DeepEqual(got, []string{"http://127.0.0.1:28100"}) {
			t.Fatalf("expected forced relay for base_url, got %v", got)
		}

		authMulti := &coreauth.Auth{
			ID:         "m4",
			Attributes: map[string]string{"base_urls": "https://daily-cloudcode-pa.googleapis.com,http://127.0.0.1:29999/"},
		}
		if got := antigravityModelBaseURLs(authMulti); !reflect.DeepEqual(got, []string{"http://127.0.0.1:28100", "http://127.0.0.1:29999"}) {
			t.Fatalf("expected forced relay in base_urls, got %v", got)
		}
	})
}
