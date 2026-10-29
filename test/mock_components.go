package test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
)

// MockProviderServer simulates an upstream AI provider.
type MockProviderServer struct {
	server       *httptest.Server
	name         string
	mu           sync.Mutex
	requestCount int32
	bodyHashes   []string
	models       []string
	status       int
	responseBody string
	hasAudio     bool
	hasImage     bool
	hasVideo     bool
}

func NewMockProviderServer(name string, defaultStatus int, defaultBody string) *MockProviderServer {
	m := &MockProviderServer{
		name:         name,
		status:       defaultStatus,
		responseBody: defaultBody,
		bodyHashes:   make([]string, 0),
		models:       make([]string, 0),
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&m.requestCount, 1)

		bodyBytes, _ := io.ReadAll(r.Body)
		h := sha256.Sum256(bodyBytes)
		hashStr := hex.EncodeToString(h[:])

		m.mu.Lock()
		m.bodyHashes = append(m.bodyHashes, hashStr)

		var payload map[string]any
		_ = json.Unmarshal(bodyBytes, &payload)
		if modelName, ok := payload["model"].(string); ok {
			m.models = append(m.models, modelName)
		}

		ct := r.Header.Get("Content-Type")
		if strings.Contains(ct, "audio") || strings.Contains(string(bodyBytes), "duration") {
			m.hasAudio = true
		}
		if strings.Contains(ct, "image") || strings.Contains(string(bodyBytes), "n") {
			m.hasImage = true
		}
		if strings.Contains(string(bodyBytes), "video") {
			m.hasVideo = true
		}

		status := m.status
		respBody := m.responseBody
		m.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	})

	m.server = httptest.NewServer(handler)
	return m
}

func (m *MockProviderServer) URL() string {
	if m.server != nil {
		return m.server.URL
	}
	return ""
}

func (m *MockProviderServer) Close() {
	if m.server != nil {
		m.server.Close()
	}
}

func (m *MockProviderServer) SetResponse(status int, body string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status = status
	m.responseBody = body
}

func (m *MockProviderServer) RequestCount() int32 {
	return atomic.LoadInt32(&m.requestCount)
}

func (m *MockProviderServer) HasAudio() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hasAudio
}

func (m *MockProviderServer) HasImage() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hasImage
}

func (m *MockProviderServer) HasVideo() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hasVideo
}

// MockUsageRecorder logs usage events in-memory.
type MockUsageRecorder struct {
	mu     sync.Mutex
	events []map[string]any
}

func NewMockUsageRecorder() *MockUsageRecorder {
	return &MockUsageRecorder{
		events: make([]map[string]any, 0),
	}
}

func (r *MockUsageRecorder) Record(event map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *MockUsageRecorder) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

func (r *MockUsageRecorder) LastEvent() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == 0 {
		return nil
	}
	return r.events[len(r.events)-1]
}
