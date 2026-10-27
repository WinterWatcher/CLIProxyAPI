package executor

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestNormalizeFinalClaudeMessagesPayload_CapMaxTokens(t *testing.T) {
	// Payload with max_tokens > 8192 should be capped to 8192
	payloadOver := []byte(`{"model":"claude-3-5-sonnet","max_tokens":16384,"messages":[{"role":"user","content":"hello"}]}`)
	outOver := normalizeFinalClaudeMessagesPayload(payloadOver)
	if got := gjson.GetBytes(outOver, "max_tokens").Int(); got != 8192 {
		t.Fatalf("max_tokens over limit = %d, want 8192", got)
	}

	// Payload with max_tokens <= 8192 should be preserved
	payloadUnder := []byte(`{"model":"claude-3-5-sonnet","max_tokens":4096,"messages":[{"role":"user","content":"hello"}]}`)
	outUnder := normalizeFinalClaudeMessagesPayload(payloadUnder)
	if got := gjson.GetBytes(outUnder, "max_tokens").Int(); got != 4096 {
		t.Fatalf("max_tokens under limit = %d, want 4096", got)
	}
}

func TestNormalizeFinalClaudeMessagesPayload_RemoveTrailingAssistantPrefill(t *testing.T) {
	// Trailing assistant prefill must be removed
	payload := []byte(`{
		"model":"claude-3-5-sonnet",
		"max_tokens":4096,
		"messages":[
			{"role":"user","content":"hello"},
			{"role":"assistant","content":"Sure, here is"}
		]
	}`)
	out := normalizeFinalClaudeMessagesPayload(payload)
	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 1 {
		t.Fatalf("messages count = %d, want 1 after trailing assistant removal: %s", len(messages), out)
	}
	if got := messages[0].Get("role").String(); got != "user" {
		t.Fatalf("remaining message role = %q, want user", got)
	}
	if got := messages[0].Get("content").String(); got != "hello" {
		t.Fatalf("remaining message content = %q, want hello", got)
	}

	// Non-trailing assistant message should be preserved
	preservePayload := []byte(`{
		"model":"claude-3-5-sonnet",
		"max_tokens":4096,
		"messages":[
			{"role":"user","content":"hello"},
			{"role":"assistant","content":"hi there"},
			{"role":"user","content":"how are you?"}
		]
	}`)
	preserveOut := normalizeFinalClaudeMessagesPayload(preservePayload)
	preserveMessages := gjson.GetBytes(preserveOut, "messages").Array()
	if len(preserveMessages) != 3 {
		t.Fatalf("messages count = %d, want 3 when assistant is not trailing: %s", len(preserveMessages), preserveOut)
	}
}

func TestNormalizeFinalClaudeMessagesPayload_AddMinimalUserContinuation(t *testing.T) {
	// If trailing assistant is removed and no user message remains, add minimal user continuation
	onlyAssistantPayload := []byte(`{
		"model":"claude-3-5-sonnet",
		"max_tokens":4096,
		"messages":[
			{"role":"assistant","content":"I am ready"}
		]
	}`)
	outAssistant := normalizeFinalClaudeMessagesPayload(onlyAssistantPayload)
	messagesAssistant := gjson.GetBytes(outAssistant, "messages").Array()
	if len(messagesAssistant) != 1 {
		t.Fatalf("messages count = %d, want 1 user continuation: %s", len(messagesAssistant), outAssistant)
	}
	if got := messagesAssistant[0].Get("role").String(); got != "user" {
		t.Fatalf("message role = %q, want user", got)
	}
	if got := messagesAssistant[0].Get("content").String(); got != "continue" {
		t.Fatalf("message content = %q, want continue", got)
	}

	// Empty messages array must receive minimal user continuation
	emptyMessagesPayload := []byte(`{
		"model":"claude-3-5-sonnet",
		"max_tokens":4096,
		"messages":[]
	}`)
	outEmpty := normalizeFinalClaudeMessagesPayload(emptyMessagesPayload)
	messagesEmpty := gjson.GetBytes(outEmpty, "messages").Array()
	if len(messagesEmpty) != 1 {
		t.Fatalf("empty messages count = %d, want 1 user continuation: %s", len(messagesEmpty), outEmpty)
	}
	if got := messagesEmpty[0].Get("role").String(); got != "user" {
		t.Fatalf("message role = %q, want user", got)
	}
	if got := messagesEmpty[0].Get("content").String(); got != "continue" {
		t.Fatalf("message content = %q, want continue", got)
	}
}
