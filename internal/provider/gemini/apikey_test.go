package gemini

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAPIKeyStrategyFetch_SendsAPIKeyInHeader(t *testing.T) {
	const apiKey = "test-api-key"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-goog-api-key"); got != apiKey {
			t.Errorf("x-goog-api-key = %q, want %q", got, apiKey)
		}
		if got := r.URL.Query().Get("key"); got != "" {
			t.Errorf("key query parameter = %q, want empty", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer server.Close()

	originalModelsURL := modelsURL
	modelsURL = server.URL
	t.Cleanup(func() { modelsURL = originalModelsURL })
	t.Setenv("GEMINI_API_KEY", apiKey)

	result, err := (&APIKeyStrategy{}).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if !result.Success {
		t.Fatalf("Fetch() success = false, error = %q", result.Error)
	}
}
