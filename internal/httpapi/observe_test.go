package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func TestRequestDiagnostics(t *testing.T) {
	var output bytes.Buffer
	previous := log.Logger
	log.Logger = zerolog.New(&output)
	t.Cleanup(func() { log.Logger = previous })
	h := New(nil, time.Minute, "test-gateway-secret-00000000000001")
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/v1/orders?address=private-address", 401},
		{"GET", "/missing-private-path", 404},
		{"DELETE", "/v1/restaurants", 405},
	} {
		output.Reset()
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("X-Request-ID", "untrusted-id")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		var entry struct {
			RequestID string `json:"request_id"`
			Status    int    `json:"status"`
			Method    string `json:"method"`
		}
		if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Status != tc.status || entry.Method != tc.method || entry.RequestID == "" || entry.RequestID != w.Header().Get("X-Request-ID") || entry.RequestID == "untrusted-id" {
			t.Fatalf("incomplete request log: %s", output.String())
		}
		if strings.Contains(output.String(), "private") {
			t.Fatal("raw URL leaked to log")
		}
	}
	output.Reset()
	h = observe(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("private-panic-value") }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 500 || !strings.Contains(output.String(), "observe_test.go") || !strings.Contains(output.String(), `"status":500`) || strings.Contains(output.String(), "private-panic-value") {
		t.Fatalf("panic diagnostics: HTTP %d %s", w.Code, output.String())
	}
	output.Reset()
	a := newAdmission()
	for range maxInFlight {
		a.slots <- struct{}{}
	}
	w = httptest.NewRecorder()
	observe(a.wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("admission bypassed") }))).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 429 || !strings.Contains(output.String(), `"status":429`) || w.Header().Get("X-Request-ID") == "" {
		t.Fatal("missing admission diagnostics")
	}
}
