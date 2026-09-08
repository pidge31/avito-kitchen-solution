// Команда restaurant имитирует отдельную POS-систему и обращается к платформе только через HTTP API
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

type menuItem struct {
	ExternalID string
	Name       string
	Category   string
	Price      int64
}
type order struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updated_at"`
}
type client struct {
	base, token string
	http        *http.Client
	after       string
}
type httpError struct{ status int }

func (e httpError) Error() string { return fmt.Sprintf("platform returned HTTP %d", e.status) }

func (c *client) request(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	r, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	r.Header.Set("Authorization", "Bearer "+c.token)
	if in != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(r)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return httpError{resp.StatusCode}
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
	}
	_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 16<<20))
	return err
}

func (c *client) seed(ctx context.Context) error {
	menu := []menuItem{{"pizza-margherita", "Пицца Маргарита", "Пицца", 45000}, {"soup-chicken", "Куриный суп", "Супы", 32000}, {"lemonade", "Домашний лимонад", "Напитки", 15000}}
	for _, item := range menu {
		payload := map[string]any{"name": item.Name, "category": item.Category, "description": "Готовим после подтверждения заказа", "price_minor": item.Price, "active": true}
		if err := c.request(ctx, "PUT", "/v1/partner/menu/items/"+item.ExternalID, payload, nil); err != nil {
			return err
		}
		if err := c.request(ctx, "POST", "/v1/partner/inventory-adjustments", map[string]any{"external_id": "bootstrap-v1-" + item.ExternalID, "item_external_id": item.ExternalID, "delta": 100}, nil); err != nil {
			return err
		}
	}
	return c.request(ctx, "PATCH", "/v1/partner/restaurant", map[string]bool{"is_open": true}, nil)
}

func (c *client) poll(ctx context.Context, delay time.Duration, reject bool) error {
	var page struct {
		Items []order `json:"items"`
	}
	path := "/v1/partner/orders?status=active&limit=100"
	if c.after != "" {
		path += "&after=" + c.after
	}
	if err := c.request(ctx, "GET", path, nil, &page); err != nil {
		return err
	}
	var firstError error
	for _, o := range page.Items {
		if time.Since(o.UpdatedAt) < delay {
			continue
		}
		next := map[string]string{"pending": "accepted", "accepted": "preparing", "preparing": "delivering", "delivering": "delivered"}[o.Status]
		if next == "" {
			continue
		}
		body := map[string]string{"from_status": o.Status, "to_status": next}
		if o.Status == "pending" {
			if reject {
				body["to_status"] = "rejected"
				body["reason"] = "Кухня временно не может приготовить заказ"
			} else {
				body["external_order_id"] = "pos-" + o.ID
			}
		}
		if err := c.request(ctx, "POST", "/v1/partner/orders/"+o.ID+"/transitions", body, nil); err != nil {
			var he httpError
			if !errors.As(err, &he) || he.status == 401 || he.status == 403 || he.status == 429 {
				return err
			}
			if he.status == 409 || he.status == 404 {
				continue
			}
			log.Warn().Str("order_id", o.ID).Int("http_status", he.status).Msg("order transition will retry")
			if firstError == nil {
				firstError = err
			}
			continue
		}
		log.Info().Str("order_id", o.ID).Str("status", body["to_status"]).Msg("order progressed")
	}
	c.after = ""
	if len(page.Items) == 100 {
		c.after = page.Items[len(page.Items)-1].ID
	}
	return firstError
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func main() {
	log.Logger = zerolog.New(os.Stdout).Level(zerolog.InfoLevel).With().Timestamp().Str("service", "restaurant").Logger()
	if err := run(); err != nil {
		log.Error().Err(err).Msg("restaurant stopped")
		os.Exit(1)
	}
}

func run() error {
	token := os.Getenv("RESTAURANT_API_KEY")
	if len(token) < 24 {
		return fmt.Errorf("RESTAURANT_API_KEY must contain at least 24 bytes")
	}
	delay, err := time.ParseDuration(env("STEP_DELAY", "3s"))
	if err != nil || delay < time.Second || delay > time.Hour {
		return fmt.Errorf("STEP_DELAY must be between 1s and 1h")
	}
	c := client{base: strings.TrimRight(env("PLATFORM_URL", "http://api:8080"), "/"), token: token, http: &http.Client{Timeout: 5 * time.Second}}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var ready atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			http.Error(w, "initializing or platform unavailable", 503)
			return
		}
		_, _ = io.WriteString(w, "ok")
	})
	server := &http.Server{Addr: env("HTTP_ADDR", ":8081"), Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	seeded := false
	for {
		select {
		case <-ctx.Done():
			return nil
		case err = <-errCh:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-ticker.C:
		}
		if !seeded {
			if err = c.seed(ctx); err != nil {
				log.Warn().Err(err).Msg("menu synchronization will retry")
				ready.Store(false)
				continue
			}
			seeded = true
			log.Info().Msg("menu synchronized")
		}
		if err = c.poll(ctx, delay, env("REJECT_ORDERS", "false") == "true"); err != nil {
			ready.Store(false)
			log.Warn().Err(err).Msg("poll will retry")
			continue
		}
		ready.Store(true)
	}
}
