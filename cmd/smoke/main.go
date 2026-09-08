// Команда smoke проверяет запущенные API и сервис заведения через HTTP.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
)

type restaurant struct {
	ID               string `json:"id"`
	ExternalID       string `json:"external_id"`
	City             string `json:"city"`
	IsOpen           bool   `json:"is_open"`
	DeliveryFeeMinor int64  `json:"delivery_fee_minor"`
}

type menuItem struct {
	ID             string `json:"id"`
	ExternalID     string `json:"external_id"`
	AvailableStock int64  `json:"available_stock"`
	PriceMinor     int64  `json:"price_minor"`
}

type order struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	TotalMinor int64  `json:"total_minor"`
	Events     []struct {
		Status string `json:"status"`
	} `json:"events"`
}

type page[T any] struct {
	Items []T `json:"items"`
}

type client struct {
	base       string
	user       string
	gatewayKey string
	http       *http.Client
}

func main() {
	base := os.Getenv("BASE_URL")
	if base == "" {
		base = "http://localhost:8080"
	}
	expected := os.Getenv("EXPECTED_STATUS")
	if expected == "" {
		expected = "delivered"
	}
	gatewayKey := os.Getenv("GATEWAY_API_KEY")
	if gatewayKey == "" {
		gatewayKey = "demo-gateway-secret-change-me-0001"
	}
	if err := run(base, expected, gatewayKey); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
}

func randomID() string {
	var value [16]byte
	_, _ = rand.Read(value[:])
	return hex.EncodeToString(value[:])
}

func (c *client) request(ctx context.Context, method, path string, body any, key string, out any) (int, http.Header, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("X-User-ID", c.user)
	req.Header.Set("X-Gateway-Key", c.gatewayKey)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, resp.Header, fmt.Errorf("%s %s: HTTP %d", method, path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return resp.StatusCode, resp.Header, fmt.Errorf("%s %s: decode response: %w", method, path, err)
	}
	return resp.StatusCode, resp.Header, nil
}

func pause(ctx context.Context) error {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *client) ready(ctx context.Context) (restaurant, menuItem, error) {
	var lastErr error
	for {
		var venues page[restaurant]
		_, _, err := c.request(ctx, "GET", "/v1/restaurants?limit=100", nil, "", &venues)
		lastErr = err
		if err == nil {
			for _, venue := range venues.Items {
				if venue.ExternalID != "demo-cafe" || !venue.IsOpen {
					continue
				}
				var menu page[menuItem]
				_, _, lastErr = c.request(ctx, "GET", "/v1/restaurants/"+venue.ID+"/menu", nil, "", &menu)
				if lastErr == nil {
					for _, item := range menu.Items {
						if item.ExternalID == "pizza-margherita" && item.AvailableStock > 0 {
							return venue, item, nil
						}
					}
				}
				break
			}
		}
		if err := pause(ctx); err != nil {
			if lastErr != nil {
				return restaurant{}, menuItem{}, fmt.Errorf("demo restaurant did not become ready within 60 seconds: %w", lastErr)
			}
			return restaurant{}, menuItem{}, fmt.Errorf("demo restaurant did not become ready within 60 seconds: open venue or available pizza not found")
		}
	}
}

func run(base, expected, gatewayKey string) error {
	terminal := []string{"delivered", "rejected", "expired", "cancelled"}
	if !slices.Contains(terminal, expected) {
		return fmt.Errorf("EXPECTED_STATUS must be one of %v", terminal)
	}
	c := client{base: strings.TrimRight(base, "/"), user: "smoke-" + randomID(), gatewayKey: gatewayKey, http: &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	readyCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
	venue, item, err := c.ready(readyCtx)
	cancel()
	if err != nil {
		return err
	}
	payload := map[string]any{
		"restaurant_id":    venue.ID,
		"city":             venue.City,
		"delivery_address": "ул. Тестовая, дом 10, квартира 5",
		"items":            []map[string]any{{"item_id": item.ID, "quantity": 1, "expected_price_minor": item.PriceMinor}},
	}
	key := randomID()
	var created, replay order
	code, headers, err := c.request(context.Background(), "POST", "/v1/orders", payload, key, &created)
	if err != nil {
		return err
	}
	if code != 201 || headers.Get("Idempotency-Replayed") != "false" || created.ID == "" {
		return fmt.Errorf("checkout must return HTTP 201, an order ID and Idempotency-Replayed: false")
	}
	code, headers, err = c.request(context.Background(), "POST", "/v1/orders", payload, key, &replay)
	if err != nil {
		return err
	}
	if code != 200 || replay.ID != created.ID || headers.Get("Idempotency-Replayed") != "true" {
		return fmt.Errorf("checkout retry must return HTTP 200, the same order ID and Idempotency-Replayed: true")
	}
	if created.TotalMinor != item.PriceMinor+venue.DeliveryFeeMinor {
		return fmt.Errorf("unexpected total: got %d, want %d", created.TotalMinor, item.PriceMinor+venue.DeliveryFeeMinor)
	}
	fmt.Printf("Created %s; checkout retry returned the same order.\n", created.ID)

	pollCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for {
		var current order
		if _, _, err := c.request(pollCtx, "GET", "/v1/orders/"+created.ID, nil, "", &current); err != nil {
			return err
		}
		if slices.Contains(terminal, current.Status) {
			if err := checkResult(current, expected); err != nil {
				return err
			}
			fmt.Printf("PASS: %s\n", strings.Join(expectedEvents(expected), " -> "))
			return nil
		}
		if err := pause(pollCtx); err != nil {
			return fmt.Errorf("order %s did not reach %s within 60 seconds; last status: %s", created.ID, expected, current.Status)
		}
	}
}

func expectedEvents(status string) []string {
	if status == "delivered" {
		return []string{"pending", "accepted", "preparing", "delivering", "delivered"}
	}
	return []string{"pending", status}
}

func checkResult(current order, expected string) error {
	if current.Status != expected {
		return fmt.Errorf("unexpected order status: got %s, want %s", current.Status, expected)
	}
	var actual []string
	for _, event := range current.Events {
		actual = append(actual, event.Status)
	}
	want := expectedEvents(expected)
	if !slices.Equal(actual, want) {
		return fmt.Errorf("unexpected order events: got %v, want %v", actual, want)
	}
	return nil
}
