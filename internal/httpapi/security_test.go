package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGatewayTrust(t *testing.T) {
	const secret = "test-gateway-secret-00000000000001"
	for _, tc := range []struct {
		name, configured, supplied, user string
		duplicate                        bool
		want                             int
	}{
		{"missing_key", secret, "", "victim", false, 401},
		{"wrong_key", secret, "wrong", "victim", false, 401},
		{"empty_configuration", "", "", "victim", false, 401},
		{"weak_configuration", "weak", "weak", "victim", false, 401},
		{"duplicate_identity", secret, secret, "victim", true, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := New(nil, time.Minute, tc.configured)
			for _, route := range []struct{ method, path string }{
				{"POST", "/v1/orders"}, {"GET", "/v1/orders"},
				{"GET", "/v1/orders/00000000-0000-0000-0000-000000000001"},
				{"POST", "/v1/orders/00000000-0000-0000-0000-000000000001/cancel"},
			} {
				r := httptest.NewRequest(route.method, route.path, nil)
				r.Header.Set("X-User-ID", tc.user)
				if tc.supplied != "" {
					r.Header.Set("X-Gateway-Key", tc.supplied)
				}
				if tc.duplicate {
					r.Header.Add("X-User-ID", "other")
				}
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				if w.Code != tc.want {
					t.Fatalf("%s %s: got %d, want %d", route.method, route.path, w.Code, tc.want)
				}
			}
		})
	}
}

func TestAdmissionLimitsAndRecovery(t *testing.T) {
	a := newAdmission()
	now := time.Now()
	for range int(peerBurst) {
		if !a.allow("peer", now) {
			t.Fatal("initial burst rejected")
		}
	}
	if a.allow("peer", now) {
		t.Fatal("peer burst exceeded")
	}
	if !a.allow("other", now) {
		t.Fatal("one peer exhausted another's quota")
	}
	if !a.allow("peer", now.Add(time.Second)) {
		t.Fatal("quota did not refill")
	}

	a = newAdmission()
	for i := range int(globalBurst) {
		if !a.allow(fmt.Sprint(i), now) {
			t.Fatal("global burst rejected early")
		}
	}
	if a.allow("new-peer", now) {
		t.Fatal("global quota bypassed by changing peers")
	}
	a = newAdmission()
	for i := range maxPeers {
		a.peers[fmt.Sprint(i)] = &bucket{0, now}
	}
	if a.allow("new-peer", now) {
		t.Fatal("peer table grew past its bound")
	}
	if !a.allow("new-peer", now.Add(2*time.Minute)) || len(a.peers) != 1 {
		t.Fatal("idle peers not reclaimed")
	}
}

func TestAdmissionIgnoresForwardedHeadersAndBoundsWork(t *testing.T) {
	a := newAdmission()
	a.peers["192.0.2.1"] = &bucket{tokens: -1000, last: time.Now()}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	h := a.wrap(next)
	r := httptest.NewRequest("GET", "/v1/restaurants", nil)
	r.RemoteAddr = "192.0.2.1:1234"
	r.Header.Set("X-Forwarded-For", "198.51.100.1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 429 || w.Header().Get("Retry-After") != "1" {
		t.Fatal("forwarded header bypassed rate limit")
	}

	a = newAdmission()
	h = a.wrap(next)
	for range maxInFlight {
		a.slots <- struct{}{}
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 429 {
		t.Fatal("concurrency limit exceeded")
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 204 {
		t.Fatal("health probe blocked by admission")
	}
	for range cap(a.healthSlots) {
		a.healthSlots <- struct{}{}
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 429 {
		t.Fatal("health probes bypass concurrency limits")
	}
	<-a.slots
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 204 || len(a.slots) != maxInFlight-1 {
		t.Fatal("slot was not released after a request")
	}
}
