package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestPollAdvancesPastFailedPage(t *testing.T) {
	var laterCalls, firstCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/partner/orders", func(w http.ResponseWriter, r *http.Request) {
		items := []order{}
		switch r.URL.Query().Get("after") {
		case "":
			for i := range 100 {
				items = append(items, order{ID: fmt.Sprintf("old-%03d", i), Status: "accepted"})
			}
		case "old-099":
			items = append(items, order{ID: "new", Status: "pending"})
		default:
			t.Errorf("unexpected cursor: %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	})
	mux.HandleFunc("POST /v1/partner/orders/{id}/transitions", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "new" {
			laterCalls++
			w.WriteHeader(200)
		} else {
			firstCalls++
			w.WriteHeader(500)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	c := client{base: server.URL, http: server.Client()}
	if err := c.poll(t.Context(), time.Second, false); err == nil {
		t.Fatal("expected page failure")
	}
	if err := c.poll(t.Context(), time.Second, false); err != nil {
		t.Fatal(err)
	}
	if err := c.poll(t.Context(), time.Second, false); err == nil {
		t.Fatal("failed orders were not revisited")
	}
	if laterCalls != 1 || firstCalls != 200 {
		t.Fatalf("later=%d old=%d", laterCalls, firstCalls)
	}
}

func TestPollFailureIsolation(t *testing.T) {
	for _, tc := range []struct {
		name          string
		status        int
		continueBatch bool
		wantError     bool
	}{
		{"server_error", 500, true, true},
		{"invalid_order", 400, true, true},
		{"stale_order", 404, true, false},
		{"conflict", 409, true, false},
		{"authentication", 401, false, true},
		{"authorization", 403, false, true},
		{"rate_limit", 429, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var firstCalls, secondCalls atomic.Int32
			mux := http.NewServeMux()
			mux.HandleFunc("GET /v1/partner/orders", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"items":[{"id":"first","status":"pending","updated_at":"2020-01-01T00:00:00Z"},{"id":"second","status":"pending","updated_at":"2020-01-01T00:00:00Z"}]}`))
			})
			mux.HandleFunc("POST /v1/partner/orders/first/transitions", func(w http.ResponseWriter, _ *http.Request) {
				firstCalls.Add(1)
				w.WriteHeader(tc.status)
			})
			mux.HandleFunc("POST /v1/partner/orders/second/transitions", func(w http.ResponseWriter, _ *http.Request) {
				secondCalls.Add(1)
				w.WriteHeader(200)
			})
			server := httptest.NewServer(mux)
			defer server.Close()
			c := client{base: server.URL, token: "test-partner-key", http: server.Client()}
			for range 2 {
				if err := c.poll(t.Context(), time.Second, false); (err != nil) != tc.wantError {
					t.Fatalf("poll error=%v; want error=%v", err, tc.wantError)
				}
			}
			wantSecond := int32(0)
			if tc.continueBatch {
				wantSecond = 2
			}
			if firstCalls.Load() != 2 || secondCalls.Load() != wantSecond {
				t.Fatalf("calls: first=%d second=%d; want second=%d", firstCalls.Load(), secondCalls.Load(), wantSecond)
			}
		})
	}
}
