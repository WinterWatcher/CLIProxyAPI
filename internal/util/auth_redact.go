package util

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// IsSensitiveAuthIdentifier reports whether an auth identifier or credential string
// contains sensitive information such as an email address, .json file name, file path,
// Bearer token, sk- API key, refresh token, or cookie.
func IsSensitiveAuthIdentifier(raw string) bool {
	s := strings.TrimSpace(raw)
	if s == "" || s == "nil" {
		return false
	}
	// Email addresses
	if strings.Contains(s, "@") {
		return true
	}
	// JSON credential file names or paths
	lower := strings.ToLower(s)
	if strings.Contains(lower, ".json") {
		return true
	}
	// Path separators indicating filesystem path
	if strings.Contains(s, "/") || strings.Contains(s, "\\") {
		return true
	}
	// Secret key or token prefixes
	if strings.HasPrefix(lower, "sk-") || strings.Contains(lower, "sk-") {
		return true
	}
	if strings.HasPrefix(lower, "bearer ") || strings.Contains(lower, "bearer ") {
		return true
	}
	if strings.Contains(lower, "refresh_token") || strings.Contains(lower, "refreshtoken") {
		return true
	}
	if strings.Contains(lower, "cookie") || strings.Contains(lower, "session=") || strings.Contains(lower, "cf_clearance") {
		return true
	}
	if strings.Contains(lower, "auths") || strings.Contains(lower, "credentials") {
		return true
	}
	return false
}

// RedactAuthIdentifier redacts email addresses, .json files, paths, API keys, tokens,
// and cookies into a stable, irreversible short hash with "auth-" prefix, while preserving
// safe auth indices and sanitized credential labels.
func RedactAuthIdentifier(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" || s == "nil" {
		return s
	}
	if !IsSensitiveAuthIdentifier(s) {
		return s
	}
	// Compute stable, irreversible short hash (8 hex chars = 32 bits from SHA256)
	sum := sha256.Sum256([]byte(s))
	return "auth-" + hex.EncodeToString(sum[:4])
}
