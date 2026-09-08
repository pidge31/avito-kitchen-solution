package httpapi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"avito-kitchen/internal/domain"
)

func TestIntegrationOverlappingBaskets(t *testing.T) {
	f := setup(t, 1)
	active := true
	second, err := f.s.PutMenu(t.Context(), f.p.ID, "second", domain.MenuInput{Name: "Second", Category: "Pizza", PriceMinor: 45000, Active: &active})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.s.AdjustInventory(t.Context(), f.p.ID, domain.Adjustment{ExternalID: "second-stock", ItemExternalID: "second", Delta: 1}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan response, 12)
	for i := range 12 {
		go func() {
			in := f.input()
			in.Items = append(in.Items, domain.OrderLineInput{ItemID: second.ID, Quantity: 1, ExpectedPriceMinor: 45000})
			if i%2 == 0 {
				in.Items[0], in.Items[1] = in.Items[1], in.Items[0]
			}
			<-start
			results <- f.call("POST", "/v1/orders", fmt.Sprint("buyer-", i), fmt.Sprint("basket-", i), "", in)
		}()
	}
	close(start)
	created := 0
	for range 12 {
		r := <-results
		if r.err != nil || (r.code != 201 && r.code != 409) {
			t.Fatalf("overlapping checkout: %+v", r)
		}
		if r.code == 201 {
			created++
		}
	}
	items, err := f.s.Menu(t.Context(), f.p.ID, "", "", true, 100, 0)
	if err != nil || created != 1 || len(items) != 2 {
		t.Fatalf("created=%d items=%v err=%v", created, items, err)
	}
	for _, item := range items {
		if item.AvailableStock != 0 {
			t.Fatalf("partial reservation: %+v", item)
		}
	}
}

func TestIntegrationPartnerCursor(t *testing.T) {
	f := setup(t, 4)
	orders := make([]domain.Order, 3)
	for i := range orders {
		orders[i] = f.order(f.call("POST", "/v1/orders", "u1", fmt.Sprint(i), "", f.input()), 201)
	}
	// При одинаковом времени строки не теряются: UUID используется как дополнительный ключ сортировки.
	if _, err := f.s.DB.ExecContext(t.Context(), `UPDATE orders SET created_at='2026-01-01T00:00:00Z'`); err != nil {
		t.Fatal(err)
	}
	read := func(path, token string) []domain.Order {
		r := f.call("GET", path, "", "", token, nil)
		f.expect(r, 200)
		var page struct {
			Items []domain.Order `json:"items"`
		}
		if err := json.Unmarshal(r.body, &page); err != nil {
			t.Fatal(err)
		}
		return page.Items
	}
	first := read("/v1/partner/orders?limit=1", token)
	if len(first) != 1 {
		t.Fatal("missing first page")
	}
	f.expect(f.transition(first[0].ID, "pending", "rejected"), 200)
	remaining := read("/v1/partner/orders?after="+first[0].ID, token)
	if len(remaining) != 2 || remaining[0].ID <= first[0].ID || remaining[1].ID <= remaining[0].ID {
		t.Fatalf("cursor skipped/repeated rows: %v", remaining)
	}
	if len(read("/v1/partner/orders?after="+first[0].ID, otherToken)) != 0 {
		t.Fatal("cursor exposed another restaurant's orders")
	}
	for _, query := range []string{"after=bad", "after=", "after=" + first[0].ID + "&offset=1", "after=" + first[0].ID + "&after=" + first[0].ID} {
		f.expect(f.call("GET", "/v1/partner/orders?"+query, "", "", token, nil), 400)
	}
}

func TestIntegrationHTTPContract(t *testing.T) {
	f := setup(t, 4)
	for _, path := range []string{"/healthz", "/v1/restaurants", "/v1/restaurants/" + f.p.ID, "/v1/restaurants/" + f.p.ID + "/menu", "/v1/partner/restaurant", "/v1/partner/menu"} {
		f.expect(f.call("GET", path, "", "", token, nil), 200)
	}
	for _, description := range []any{nil, "", "null", "Описание"} {
		want := 200
		if description == nil {
			want = 400
		}
		f.expect(f.call("PUT", "/v1/partner/menu/items/pizza", "", "", token, map[string]any{
			"name": "Pizza", "category": "Pizza", "price_minor": 45000, "active": true, "description": description,
		}), want)
	}
	req, _ := http.NewRequest("GET", f.server.URL+"/v1/restaurants", nil)
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}}
	if err := f.validateExchange(req, resp, []byte(`{"items":"broken","limit":20,"offset":0}`)); err == nil {
		t.Fatal("contract check accepted an invalid response")
	}
	req, _ = http.NewRequest("PUT", f.server.URL+"/v1/partner/menu/items/pizza", strings.NewReader(`{"name":"Pizza","category":"Pizza","price_minor":45000,"active":true,"description":null}`))
	req.Header.Set("Content-Type", "application/json")
	if err := f.validateExchange(req, resp, []byte(`{}`)); err == nil || !strings.Contains(err.Error(), "successful request violates") {
		t.Fatalf("contract check did not detect invalid accepted input: %v", err)
	}
}
