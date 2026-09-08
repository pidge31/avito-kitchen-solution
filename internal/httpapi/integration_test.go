package httpapi_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"avito-kitchen/internal/domain"
	"avito-kitchen/internal/httpapi"
	"avito-kitchen/internal/store"
	"avito-kitchen/migrations"
	"avito-kitchen/openapi"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"
)

const token = "integration-restaurant-secret-0001"
const otherToken = "integration-restaurant-secret-0002"
const gatewayToken = "integration-gateway-secret-00000001"

type fixture struct {
	t      *testing.T
	s      *store.Store
	server *httptest.Server
	p      domain.Restaurant
	m      domain.MenuItem
	router routers.Router
}
type response struct {
	code   int
	header http.Header
	body   []byte
	err    error
}

func randomID() string { var b [8]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }

func setup(t *testing.T, stock int) *fixture {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	admin, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "test_" + randomID()
	if _, err = admin.DB.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.DB.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		_ = admin.DB.Close()
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	s, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.DB.Close() })
	if err = migrations.Run(ctx, s.DB, "up"); err != nil {
		t.Fatal(err)
	}
	if err = migrations.Run(ctx, s.DB, "up"); err != nil {
		t.Fatalf("migration replay: %v", err)
	}
	p, err := s.Provision(ctx, "test-cafe", "Тестовое кафе", "Москва", "Пицца", token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Provision(ctx, "other-cafe", "Другое кафе", "Москва", "Супы", otherToken); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetOpen(ctx, p.ID, true); err != nil {
		t.Fatal(err)
	}
	active := true
	m, err := s.PutMenu(ctx, p.ID, "pizza", domain.MenuInput{Name: "Пицца", Category: "Пицца", PriceMinor: 45000, Active: &active})
	if err != nil {
		t.Fatal(err)
	}
	if stock > 0 {
		if _, _, err = s.AdjustInventory(ctx, p.ID, domain.Adjustment{ExternalID: "initial", ItemExternalID: "pizza", Delta: stock}); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(httpapi.New(s, time.Minute, gatewayToken))
	t.Cleanup(server.Close)
	spec, err := openapi3.NewLoader().LoadFromData(openapi.Spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.Servers = nil
	router, err := legacy.NewRouter(spec)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, s: s, server: server, p: p, m: m, router: router}
}

func (f *fixture) call(method, path, user, key, auth string, body any) response {
	var b io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return response{err: err}
		}
		b = bytes.NewReader(raw)
	}
	r, err := http.NewRequest(method, f.server.URL+path, b)
	if err != nil {
		return response{err: err}
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Gateway-Key", gatewayToken)
	if user != "" {
		r.Header.Set("X-User-ID", user)
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	if auth != "" {
		r.Header.Set("Authorization", "Bearer "+auth)
	}
	c := http.Client{Timeout: 20 * time.Second}
	resp, err := c.Do(r)
	if err != nil {
		return response{err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err == nil {
		err = f.validateExchange(r, resp, raw)
	}
	return response{resp.StatusCode, resp.Header, raw, err}
}

func (f *fixture) validateExchange(req *http.Request, resp *http.Response, body []byte) error {
	route, params, err := f.router.FindRoute(req)
	if err != nil {
		if resp.StatusCode == 404 || resp.StatusCode == 405 {
			return nil
		}
		return err
	}
	clone := req.Clone(req.Context())
	if req.GetBody != nil {
		clone.Body, err = req.GetBody()
		if err != nil {
			return err
		}
		defer func() { _ = clone.Body.Close() }()
	}
	options := &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc, IncludeResponseStatus: true}
	input := &openapi3filter.RequestValidationInput{Request: clone, PathParams: params, Route: route, Options: options}
	if resp.StatusCode < 400 {
		if err := openapi3filter.ValidateRequest(req.Context(), input); err != nil {
			return fmt.Errorf("successful request violates OpenAPI: %w", err)
		}
	}
	return openapi3filter.ValidateResponse(req.Context(), &openapi3filter.ResponseValidationInput{
		RequestValidationInput: input, Status: resp.StatusCode, Header: resp.Header,
		Body: io.NopCloser(bytes.NewReader(body)), Options: options,
	})
}
func (f *fixture) expect(resp response, status int) {
	f.t.Helper()
	if resp.err != nil || resp.code != status {
		f.t.Fatalf("HTTP %d, want %d: %s, err=%v", resp.code, status, resp.body, resp.err)
	}
}
func (f *fixture) order(resp response, status int) domain.Order {
	f.t.Helper()
	f.expect(resp, status)
	var o domain.Order
	if err := json.Unmarshal(resp.body, &o); err != nil {
		f.t.Fatal(err)
	}
	return o
}
func (f *fixture) input() domain.CreateOrder {
	return domain.CreateOrder{RestaurantID: f.p.ID, City: "Москва", DeliveryAddress: "ул. Лесная, дом 12, квартира 3", Items: []domain.OrderLineInput{{ItemID: f.m.ID, Quantity: 1, ExpectedPriceMinor: 45000}}}
}
func (f *fixture) stock() int {
	f.t.Helper()
	items, err := f.s.Menu(context.Background(), f.p.ID, "", "", true, 20, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	return items[0].AvailableStock
}
func (f *fixture) transition(id, from, to string) response {
	in := domain.Transition{FromStatus: from, ToStatus: to}
	if to == "accepted" {
		in.ExternalOrderID = "pos-" + id
	}
	if to == "rejected" {
		in.Reason = "Нет ингредиентов"
	}
	return f.call("POST", "/v1/partner/orders/"+id+"/transitions", "", "", token, in)
}

func TestIntegrationLifecycle(t *testing.T) {
	f := setup(t, 5)
	ctx := context.Background()
	in := f.input()
	r := f.call("POST", "/v1/orders", "u1", "order-1", "", in)
	o := f.order(r, 201)
	if r.header.Get("Location") != "/v1/orders/"+o.ID || o.TotalMinor != 64900 || len(o.Events) != 1 {
		t.Fatalf("bad created order: %+v", o)
	}
	if f.stock() != 4 {
		t.Fatal("stock not reserved")
	}
	r = f.call("POST", "/v1/orders", "u1", "order-1", "", in)
	replay := f.order(r, 200)
	if replay.ID != o.ID || r.header.Get("Idempotency-Replayed") != "true" || f.stock() != 4 {
		t.Fatal("non-idempotent checkout")
	}
	in.Items[0].Quantity = 2
	f.expect(f.call("POST", "/v1/orders", "u1", "order-1", "", in), 409)
	f.expect(f.call("GET", "/v1/orders/"+o.ID, "u2", "", "", nil), 404)
	f.expect(f.call("GET", "/v1/partner/orders/"+o.ID, "", "", otherToken, nil), 404)
	f.expect(f.call("POST", "/v1/partner/orders/"+o.ID+"/transitions", "", "", otherToken, domain.Transition{FromStatus: "pending", ToStatus: "accepted", ExternalOrderID: "foreign"}), 404)
	f.expect(f.call("GET", "/v1/partner/menu", "", "", "", nil), 401)
	f.expect(f.call("GET", "/v1/partner/menu", "", "", "not-a-valid-key-0000000000", nil), 401)
	f.expect(f.transition(o.ID, "pending", "accepted"), 200)
	f.expect(f.transition(o.ID, "pending", "accepted"), 200)
	f.expect(f.call("POST", "/v1/orders/"+o.ID+"/cancel", "u1", "", "", nil), 409)
	for _, step := range [][2]string{{"accepted", "preparing"}, {"preparing", "delivering"}, {"delivering", "delivered"}} {
		f.expect(f.transition(o.ID, step[0], step[1]), 200)
	}
	f.expect(f.transition(o.ID, "pending", "accepted"), 409)
	active := true
	if _, err := f.s.PutMenu(ctx, f.p.ID, "pizza", domain.MenuInput{Name: "Новое название", Category: "Пицца", PriceMinor: 55000, Active: &active}); err != nil {
		t.Fatal(err)
	}
	o = f.order(f.call("GET", "/v1/orders/"+o.ID, "u1", "", "", nil), 200)
	if o.Status != "delivered" || len(o.Events) != 5 || o.Items[0].Name != "Пицца" || o.Items[0].PriceMinor != 45000 || f.stock() != 4 {
		t.Fatalf("snapshot/history broken: %+v", o)
	}
}

func TestIntegrationRollbackAndAvailability(t *testing.T) {
	f := setup(t, 3)
	ctx := context.Background()
	in := f.input()
	in.Items[0].ExpectedPriceMinor = 44000
	f.expect(f.call("POST", "/v1/orders", "u1", "price", "", in), 409)
	in = f.input()
	in.City = "Казань"
	f.expect(f.call("POST", "/v1/orders", "u1", "city", "", in), 409)
	in = f.input()
	in.Items[0].Quantity = 4
	f.expect(f.call("POST", "/v1/orders", "u1", "stock", "", in), 409)
	if _, err := f.s.SetOpen(ctx, f.p.ID, false); err != nil {
		t.Fatal(err)
	}
	f.expect(f.call("POST", "/v1/orders", "u1", "closed", "", f.input()), 409)
	if _, err := f.s.SetOpen(ctx, f.p.ID, true); err != nil {
		t.Fatal(err)
	}
	active := true
	second, err := f.s.PutMenu(ctx, f.p.ID, "soup", domain.MenuInput{Name: "Суп", Category: "Супы", PriceMinor: 32000, Active: &active})
	if err != nil {
		t.Fatal(err)
	}
	in = f.input()
	in.Items = append(in.Items, domain.OrderLineInput{ItemID: second.ID, Quantity: 1, ExpectedPriceMinor: 32000})
	f.expect(f.call("POST", "/v1/orders", "u1", "partial", "", in), 409)
	var count int
	if err = f.s.DB.QueryRowContext(ctx, `SELECT count(*) FROM orders`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("failed checkout left an order")
	}
	var remaining int
	if err = f.s.DB.QueryRowContext(ctx, `SELECT available_stock FROM menu_items WHERE id=$1`, f.m.ID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 3 {
		t.Fatal("failed checkout changed stock")
	}
	f.expect(f.call("POST", "/v1/orders", "u1", "price", "", f.input()), 201)
}

func TestIntegrationReleaseAndInventoryEvents(t *testing.T) {
	f := setup(t, 10)
	ctx := context.Background()
	for _, target := range []string{"cancelled", "rejected", "expired"} {
		o := f.order(f.call("POST", "/v1/orders", "u1", target, "", f.input()), 201)
		switch target {
		case "cancelled":
			for range 2 {
				f.expect(f.call("POST", "/v1/orders/"+o.ID+"/cancel", "u1", "", "", nil), 200)
			}
		case "rejected":
			for range 2 {
				f.expect(f.transition(o.ID, "pending", "rejected"), 200)
			}
		case "expired":
			if _, err := f.s.DB.ExecContext(ctx, `UPDATE orders SET expires_at=now()-interval '1 second' WHERE id=$1`, o.ID); err != nil {
				t.Fatal(err)
			}
			f.expect(f.transition(o.ID, "pending", "accepted"), 409)
			changed, err := f.s.ExpireOne(ctx)
			if err != nil || !changed {
				t.Fatalf("expiration failed %v %v", changed, err)
			}
			changed, err = f.s.ExpireOne(ctx)
			if err != nil || changed {
				t.Fatal("expiration replay changed data")
			}
		}
		o = f.order(f.call("GET", "/v1/orders/"+o.ID, "u1", "", "", nil), 200)
		if o.Status != target || len(o.Events) != 2 || f.stock() != 10 {
			t.Fatalf("release broken for %s", target)
		}
	}
	a := domain.Adjustment{ExternalID: "restock-1", ItemExternalID: "pizza", Delta: 3}
	for range 2 {
		f.expect(f.call("POST", "/v1/partner/inventory-adjustments", "", "", token, a), 200)
	}
	if f.stock() != 13 {
		t.Fatal("inventory replay added stock")
	}
	a.Delta = 4
	f.expect(f.call("POST", "/v1/partner/inventory-adjustments", "", "", token, a), 409)
	a.ExternalID = "negative"
	a.Delta = -14
	f.expect(f.call("POST", "/v1/partner/inventory-adjustments", "", "", token, a), 409)
	if f.stock() != 13 {
		t.Fatal("negative adjustment changed stock")
	}
}

func TestIntegrationConcurrentCheckout(t *testing.T) {
	for _, sameKey := range []bool{false, true} {
		t.Run(fmt.Sprint(sameKey), func(t *testing.T) {
			f := setup(t, 1)
			const workers = 12
			results := make(chan response, workers)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := range workers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					key := fmt.Sprintf("key-%d", i)
					user := fmt.Sprintf("user-%d", i)
					if sameKey {
						key = "same"
						user = "u1"
					}
					results <- f.call("POST", "/v1/orders", user, key, "", f.input())
				}()
			}
			close(start)
			wg.Wait()
			close(results)
			created, replayed, conflicts := 0, 0, 0
			id := ""
			for r := range results {
				if r.err != nil {
					t.Fatal(r.err)
				}
				switch r.code {
				case 201:
					created++
				case 200:
					replayed++
				case 409:
					conflicts++
				default:
					t.Fatalf("unexpected response %d: %s", r.code, r.body)
				}
				if r.code == 200 || r.code == 201 {
					var o domain.Order
					if err := json.Unmarshal(r.body, &o); err != nil {
						t.Fatal(err)
					}
					if id != "" && o.ID != id {
						t.Fatal("duplicate order")
					}
					id = o.ID
				}
			}
			if created != 1 || f.stock() != 0 || (sameKey && replayed != workers-1) || (!sameKey && conflicts != workers-1) {
				t.Fatalf("created=%d replayed=%d conflicts=%d stock=%d", created, replayed, conflicts, f.stock())
			}
		})
	}
}

func TestIntegrationCancelAcceptRace(t *testing.T) {
	f := setup(t, 1)
	for i := range 8 {
		o := f.order(f.call("POST", "/v1/orders", "u1", fmt.Sprint(i), "", f.input()), 201)
		start := make(chan struct{})
		out := make(chan response, 2)
		go func() { <-start; out <- f.transition(o.ID, "pending", "accepted") }()
		go func() { <-start; out <- f.call("POST", "/v1/orders/"+o.ID+"/cancel", "u1", "", "", nil) }()
		close(start)
		a, b := <-out, <-out
		if a.err != nil || b.err != nil || !((a.code == 200 && b.code == 409) || (a.code == 409 && b.code == 200)) {
			t.Fatalf("race responses: %+v %+v", a, b)
		}
		o = f.order(f.call("GET", "/v1/orders/"+o.ID, "u1", "", "", nil), 200)
		if len(o.Events) != 2 {
			t.Fatal("both commands succeeded")
		}
		if o.Status == "accepted" {
			if f.stock() != 0 {
				t.Fatal("accepted order's reservation was released")
			}
			for _, step := range [][2]string{{"accepted", "preparing"}, {"preparing", "delivering"}, {"delivering", "delivered"}} {
				f.expect(f.transition(o.ID, step[0], step[1]), 200)
			}
			if _, _, err := f.s.AdjustInventory(context.Background(), f.p.ID, domain.Adjustment{ExternalID: "add-" + fmt.Sprint(i), ItemExternalID: "pizza", Delta: 1}); err != nil {
				t.Fatal(err)
			}
		} else if o.Status != "cancelled" || f.stock() != 1 {
			t.Fatal("invalid cancel outcome")
		}
	}
}

func TestIntegrationHTTPValidationAndMigration(t *testing.T) {
	f := setup(t, 2)
	f.expect(f.call("POST", "/v1/orders", "", "key", "", f.input()), 400)
	f.expect(f.call("POST", "/v1/orders", "u1", "", "", f.input()), 400)
	f.expect(f.call("GET", "/v1/orders/not-a-uuid", "u1", "", "", nil), 400)
	f.expect(f.call("GET", "/v1/restaurants?limit=-1", "", "", "", nil), 400)
	f.expect(f.call("GET", "/v1/orders?status=oops", "u1", "", "", nil), 400)
	f.expect(f.call("PATCH", "/v1/partner/restaurant", "", "", token, map[string]any{}), 400)
	f.expect(f.call("GET", "/unknown", "", "", "", nil), 404)
	f.expect(f.call("DELETE", "/v1/restaurants", "", "", "", nil), 405)
	for _, body := range []string{`{"extra":true}`, `{} {}`, `null`} {
		r, err := http.NewRequest("POST", f.server.URL+"/v1/orders", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-User-ID", "u1")
		r.Header.Set("X-Gateway-Key", gatewayToken)
		r.Header.Set("Idempotency-Key", "json")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("body %s returned %d", body, resp.StatusCode)
		}
	}
	ctx := context.Background()
	if err := migrations.Run(ctx, f.s.DB, "down"); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Run(ctx, f.s.DB, "up"); err != nil {
		t.Fatal(err)
	}
}
