package httpserver

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Gursevak56/food-delivery-platform/services/loyalty-service/config"
)

func TestFoundationRoutes(t *testing.T) {
	router, err := NewRouter(config.Config{Environment: "test", LogLevel: "ERROR"}, discardLogger())
	if err != nil {
		t.Fatalf("create router: %v", err)
	}

	tests := []struct {
		path       string
		wantStatus int
		wantBody   map[string]string
	}{
		{path: "/health", wantStatus: http.StatusOK, wantBody: map[string]string{"status": "ok"}},
		{path: "/ready", wantStatus: http.StatusOK, wantBody: map[string]string{"status": "ready", "mode": "foundation"}},
		{path: "/v1/loyalty/wallet", wantStatus: http.StatusNotFound, wantBody: map[string]string{"error": "not_found"}},
	}

	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			var body map[string]string
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			for key, value := range test.wantBody {
				if body[key] != value {
					t.Fatalf("body[%q] = %q, want %q", key, body[key], value)
				}
			}
			if response.Header().Get(requestIDHeader) == "" {
				t.Fatal("response omitted request ID")
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("response omitted no-store cache policy")
			}
		})
	}
}

func TestRequestIDPolicy(t *testing.T) {
	router, err := NewRouter(config.Config{Environment: "test", LogLevel: "ERROR"}, discardLogger())
	if err != nil {
		t.Fatalf("create router: %v", err)
	}

	t.Run("valid ID is retained", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/health", nil)
		request.Header.Set(requestIDHeader, "checkout-123")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if got := response.Header().Get(requestIDHeader); got != "checkout-123" {
			t.Fatalf("request ID = %q, want checkout-123", got)
		}
	})

	t.Run("invalid ID is replaced", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/health", nil)
		request.Header.Set(requestIDHeader, "contains spaces and unsafe input")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if got := response.Header().Get(requestIDHeader); got == "" || got == "contains spaces and unsafe input" {
			t.Fatalf("invalid request ID was not replaced: %q", got)
		}
	})
}

func TestReadAPIRequiresReader(t *testing.T) {
	_, err := NewRouter(config.Config{
		Environment: "test",
		LogLevel:    "ERROR",
		Features:    config.FeatureFlags{ReadAPI: true},
	}, discardLogger())
	if err == nil {
		t.Fatal("read API started without a wallet reader")
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
