package util

import (
	"strings"
	"testing"
)

func TestRedactAuthIdentifier(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		forbidden []string
		wantExact string
	}{
		{
			name:      "email address",
			input:     "user@gmail.com",
			forbidden: []string{"gmail.com", "user@"},
		},
		{
			name:      "json filename with email",
			input:     "alice@example.com.json",
			forbidden: []string{"example.com", ".json", "alice@"},
		},
		{
			name:      "arbitrary json credential file",
			input:     "oauth_secret_credential_12345.json",
			forbidden: []string{".json", "oauth_secret_credential_12345"},
		},
		{
			name:      "full oauth file path",
			input:     "/opt/cliproxy/auths/oauth/my_account.json",
			forbidden: []string{"/opt/cliproxy", "auths/oauth", ".json", "my_account"},
		},
		{
			name:      "bearer token",
			input:     "Bearer eyJhbGciOiJSUzI1NiIsImtpZCI6IjEyMzQ1In0",
			forbidden: []string{"Bearer", "eyJhbGciOiJSUzI1NiIsImtpZCI6IjEyMzQ1In0"},
		},
		{
			name:      "sk api key",
			input:     "sk-ant-api03-abcdef1234567890abcdef1234567890",
			forbidden: []string{"sk-", "abcdef1234567890abcdef1234567890"},
		},
		{
			name:      "refresh token",
			input:     "refresh_token_sec_99999",
			forbidden: []string{"refresh_token", "sec_99999"},
		},
		{
			name:      "preserves clean auth_index",
			input:     "auth-index-7",
			wantExact: "auth-index-7",
		},
		{
			name:      "preserves clean credential label",
			input:     "codex-auth-1",
			wantExact: "codex-auth-1",
		},
		{
			name:      "preserves empty string",
			input:     "",
			wantExact: "",
		},
		{
			name:      "preserves nil label",
			input:     "nil",
			wantExact: "nil",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactAuthIdentifier(tc.input)
			if tc.wantExact != "" {
				if got != tc.wantExact {
					t.Fatalf("RedactAuthIdentifier(%q) = %q, want %q", tc.input, got, tc.wantExact)
				}
			}
			for _, f := range tc.forbidden {
				if strings.Contains(got, f) {
					t.Fatalf("RedactAuthIdentifier(%q) leaked forbidden token %q: got %q", tc.input, f, got)
				}
			}
			if len(tc.forbidden) > 0 {
				// Must be non-empty and start with auth-
				if !strings.HasPrefix(got, "auth-") {
					t.Fatalf("expected redacted hash with prefix auth-, got: %q", got)
				}
				// Deterministic: multiple calls on same input yield identical hash
				gotAgain := RedactAuthIdentifier(tc.input)
				if got != gotAgain {
					t.Fatalf("non-deterministic output for %q: %q vs %q", tc.input, got, gotAgain)
				}
			}
		})
	}
}
