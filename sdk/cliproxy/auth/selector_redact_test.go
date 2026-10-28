package auth

import (
	"context"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestSelectorAndConductorLogsRedactSensitiveAuthInfo(t *testing.T) {
	logger := log.StandardLogger()
	previousHooks := logger.ReplaceHooks(make(log.LevelHooks))
	previousLevel := logger.GetLevel()
	hook := logtest.NewLocal(logger)
	logger.SetLevel(log.DebugLevel)
	t.Cleanup(func() {
		logger.ReplaceHooks(previousHooks)
		logger.SetLevel(previousLevel)
	})

	const (
		sensitiveEmail    = "developer@gmail.com"
		sensitiveFileName = "/opt/cliproxy/auths/oauth/developer@gmail.com.json"
		sensitiveAuthID   = "developer@gmail.com.json"
		sensitiveKey      = "sk-proj-super-secret-12345"
		safeAuthIndex     = "auth-index-99"
		testProvider      = "test-provider"
		testModel         = "test-model"
		testSessionID     = "session-xyz-12345678"
	)

	// 1. Test Selector Session-Affinity logging with sensitive Auth
	t.Run("session-affinity formatted log desensitization", func(t *testing.T) {
		hook.Reset()
		auth := &Auth{
			ID:       sensitiveAuthID,
			Index:    safeAuthIndex,
			Provider: testProvider,
			FileName: sensitiveFileName,
			Status:   StatusActive,
		}

		selector := NewSessionAffinitySelector(&RoundRobinSelector{})

		ctx := context.Background()
		opts := cliproxyexecutor.Options{
			Metadata: map[string]any{
				cliproxyexecutor.ExecutionSessionMetadataKey: testSessionID,
			},
		}

		// First pick -> cache miss, new binding
		picked, err := selector.Pick(ctx, testProvider, testModel, opts, []*Auth{auth})
		if err != nil || picked == nil {
			t.Fatalf("Pick failed: auth=%v, err=%v", picked, err)
		}

		// Second pick -> cache hit
		picked2, err := selector.Pick(ctx, testProvider, testModel, opts, []*Auth{auth})
		if err != nil || picked2 == nil {
			t.Fatalf("Pick 2 failed: auth=%v, err=%v", picked2, err)
		}

		for _, entry := range hook.AllEntries() {
			msg := entry.Message
			if !strings.Contains(msg, "session-affinity:") {
				continue
			}
			// Verify forbidden strings are NOT in message
			forbidden := []string{
				"gmail.com",
				".json",
				"/opt/cliproxy",
				"auths/oauth",
				sensitiveEmail,
			}
			for _, f := range forbidden {
				if strings.Contains(msg, f) {
					t.Fatalf("session-affinity log leaked sensitive info %q: %s", f, msg)
				}
			}
			// Verify allowed fields are preserved
			if !strings.Contains(msg, testProvider) {
				t.Fatalf("session-affinity log missing provider: %s", msg)
			}
			if !strings.Contains(msg, testModel) {
				t.Fatalf("session-affinity log missing model: %s", msg)
			}
		}
	})

	// 2. Test formatAuthIdentity and formatOauthIdentity
	t.Run("formatAuthIdentity and formatOauthIdentity desensitization", func(t *testing.T) {
		authOAuth := &Auth{
			ID:       sensitiveAuthID,
			Index:    safeAuthIndex,
			Provider: testProvider,
			FileName: sensitiveFileName,
			Metadata: map[string]any{
				"type":  "oauth",
				"email": sensitiveEmail,
			},
		}

		ident := formatAuthIdentity(authOAuth, testProvider)
		forbidden := []string{
			"gmail.com",
			".json",
			"/opt/cliproxy",
			"auths/oauth",
			sensitiveEmail,
		}
		for _, f := range forbidden {
			if strings.Contains(ident, f) {
				t.Fatalf("formatAuthIdentity leaked %q: %s", f, ident)
			}
		}

		authKey := &Auth{
			ID:       "key-auth-1",
			Provider: testProvider,
			Attributes: map[string]string{
				AttributeAPIKey: sensitiveKey,
			},
		}
		keyIdent := formatAuthIdentity(authKey, testProvider)
		if strings.Contains(keyIdent, "sk-") {
			t.Fatalf("formatAuthIdentity leaked sk- key prefix: %s", keyIdent)
		}
	})

	// 3. Test structured fields pathway
	t.Run("structured fields desensitization", func(t *testing.T) {
		hook.Reset()
		redactedID := util.RedactAuthIdentifier(sensitiveAuthID)
		if strings.Contains(redactedID, "gmail.com") || strings.Contains(redactedID, ".json") {
			t.Fatalf("RedactAuthIdentifier leaked sensitive info: %s", redactedID)
		}

		entry := logger.WithFields(log.Fields{
			"auth":       redactedID,
			"auth_id":    redactedID,
			"provider":   testProvider,
			"model":      testModel,
			"session":    "sess-123",
			"request_id": "req-456",
		})
		entry.Info("structured auth event")

		entries := hook.AllEntries()
		if len(entries) == 0 {
			t.Fatal("no log entry recorded")
		}
		e := entries[len(entries)-1]
		authVal, _ := e.Data["auth"].(string)
		authIDVal, _ := e.Data["auth_id"].(string)

		for _, val := range []string{authVal, authIDVal} {
			if strings.Contains(val, "gmail.com") || strings.Contains(val, ".json") {
				t.Fatalf("structured log field leaked: %s", val)
			}
			if !strings.HasPrefix(val, "auth-") {
				t.Fatalf("expected short hash starting with auth-, got: %s", val)
			}
		}
		if e.Data["provider"] != testProvider || e.Data["model"] != testModel || e.Data["request_id"] != "req-456" {
			t.Fatalf("allowed fields corrupted in structured log: %#v", e.Data)
		}
	})
}
