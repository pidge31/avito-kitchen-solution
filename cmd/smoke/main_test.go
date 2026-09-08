package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSmokeProtocol(t *testing.T) {
	for _, tc := range []struct {
		name, expected, wantError string
	}{
		{"delivered", "delivered", ""},
		{"rejected", "rejected", ""},
		{"duplicate_order", "delivered", "checkout retry"},
		{"wrong_total", "delivered", "unexpected total"},
		{"missing_event", "delivered", "unexpected order events"},
		{"wrong_status", "delivered", "unexpected order status"},
		{"http_error", "delivered", "HTTP 500"},
		{"redirect", "delivered", "HTTP 307"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var firstKey, firstUser string
			posts := 0
			mux := http.NewServeMux()
			mux.HandleFunc("/redirect-target", func(w http.ResponseWriter, _ *http.Request) {
				t.Error("smoke followed a redirect with gateway credentials")
				w.WriteHeader(500)
			})
			mux.HandleFunc("GET /v1/restaurants", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("limit") != "100" {
					t.Error("missing restaurant pagination")
				}
				_, _ = w.Write([]byte(`{"items":[{"id":"venue","external_id":"demo-cafe","city":"Москва","is_open":true,"delivery_fee_minor":5000}]}`))
			})
			mux.HandleFunc("GET /v1/restaurants/venue/menu", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"items":[{"id":"pizza","external_id":"pizza-margherita","available_stock":1,"price_minor":45000}]}`))
			})
			mux.HandleFunc("POST /v1/orders", func(w http.ResponseWriter, r *http.Request) {
				if tc.name == "redirect" {
					http.Redirect(w, r, "/redirect-target", http.StatusTemporaryRedirect)
					return
				}
				if r.Header.Get("X-Gateway-Key") != "test-gateway-secret-00000000000001" {
					t.Error("missing gateway credential")
				}
				if tc.name == "http_error" {
					w.WriteHeader(500)
					return
				}
				var body struct {
					RestaurantID    string `json:"restaurant_id"`
					City            string `json:"city"`
					DeliveryAddress string `json:"delivery_address"`
					Items           []struct {
						ItemID             string `json:"item_id"`
						Quantity           int    `json:"quantity"`
						ExpectedPriceMinor int64  `json:"expected_price_minor"`
					} `json:"items"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.RestaurantID != "venue" || body.City != "Москва" || body.DeliveryAddress == "" || len(body.Items) != 1 {
					t.Errorf("unexpected checkout payload: %+v", body)
				} else if item := body.Items[0]; item.ItemID != "pizza" || item.Quantity != 1 || item.ExpectedPriceMinor != 45000 {
					t.Errorf("unexpected checkout item: %+v", item)
				}
				if r.Header.Get("Content-Type") != "application/json" {
					t.Error("missing Content-Type")
				}
				posts++
				result := order{ID: "order-1", TotalMinor: 50000}
				if tc.name == "wrong_total" {
					result.TotalMinor++
				}
				if posts == 1 {
					firstKey, firstUser = r.Header.Get("Idempotency-Key"), r.Header.Get("X-User-ID")
					if firstKey == "" || firstUser == "" {
						t.Error("missing checkout identity headers")
					}
					w.Header().Set("Idempotency-Replayed", "false")
					w.WriteHeader(201)
				} else {
					if r.Header.Get("Idempotency-Key") != firstKey || r.Header.Get("X-User-ID") != firstUser {
						t.Error("retry changed identity headers")
					}
					w.Header().Set("Idempotency-Replayed", "true")
					if tc.name == "duplicate_order" {
						result.ID = "order-2"
					}
				}
				_ = json.NewEncoder(w).Encode(result)
			})
			mux.HandleFunc("GET /v1/orders/order-1", func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-User-ID") != firstUser {
					t.Error("poll changed user identity")
				}
				statuses := []string{"pending", "accepted", "preparing", "delivering", "delivered"}
				status := "delivered"
				if tc.name == "rejected" || tc.name == "wrong_status" {
					status, statuses = "rejected", []string{"pending", "rejected"}
				}
				if tc.name == "missing_event" {
					statuses = []string{"pending", "delivered"}
				}
				var events []map[string]string
				for _, s := range statuses {
					events = append(events, map[string]string{"status": s})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "order-1", "status": status, "events": events})
			})
			server := httptest.NewServer(mux)
			defer server.Close()
			err := run(server.URL, tc.expected, "test-gateway-secret-00000000000001")
			if tc.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("got %v; want error containing %q", err, tc.wantError)
			}
		})
	}
}
