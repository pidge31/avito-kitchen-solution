package httpapi_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"avito-kitchen/internal/domain"
	"avito-kitchen/internal/store"
	"avito-kitchen/migrations"
)

func TestIntegrationActiveOrderQuota(t *testing.T) {
	f := setup(t, 20)
	type result struct {
		response
		key string
	}
	results := make(chan result, 12)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Go(func() {
			<-start
			key := fmt.Sprint(i)
			results <- result{f.call("POST", "/v1/orders", "quota-user", key, "", f.input()), key}
		})
	}
	close(start)
	wg.Wait()
	close(results)
	created := 0
	var first domain.Order
	var firstKey string
	for r := range results {
		if r.code == 201 {
			created++
			first = f.order(r.response, 201)
			firstKey = r.key
		} else {
			f.expect(r.response, 409)
			var body struct {
				Error domain.Error `json:"error"`
			}
			if err := json.Unmarshal(r.body, &body); err != nil || body.Error.Code != "active_order_limit" {
				t.Fatalf("unexpected rejection: %s", r.body)
			}
		}
	}
	if created != store.MaxActiveOrdersPerUser || f.stock() != 20-created {
		t.Fatalf("quota raced: created=%d stock=%d", created, f.stock())
	}
	replay := f.order(f.call("POST", "/v1/orders", "quota-user", firstKey, "", f.input()), 200)
	if replay.ID != first.ID {
		t.Fatal("quota broke an idempotent replay")
	}
	f.expect(f.call("POST", "/v1/orders/"+first.ID+"/cancel", "quota-user", "", "", nil), 200)
	f.expect(f.call("POST", "/v1/orders", "quota-user", "after-cancel", "", f.input()), 201)
	if f.stock() != 20-created {
		t.Fatal("rejected checkouts changed reservations")
	}
	f.expect(f.call("POST", "/v1/orders", "another-user", "independent-quota", "", f.input()), 201)
}

func TestIntegrationOrderListScopes(t *testing.T) {
	f := setup(t, 10)
	first := f.order(f.call("POST", "/v1/orders", "u1", "one", "", f.input()), 201)
	second := f.order(f.call("POST", "/v1/orders", "u1", "two", "", f.input()), 201)
	third := f.order(f.call("POST", "/v1/orders", "u2", "three", "", f.input()), 201)
	f.expect(f.transition(second.ID, "pending", "rejected"), 200)
	for _, tc := range []struct {
		path, user, key string
		ids             []string
	}{
		{"/v1/orders", "u1", "", []string{first.ID, second.ID}},
		{"/v1/orders?status=active", "u1", "", []string{first.ID}},
		{"/v1/orders?status=rejected", "u1", "", []string{second.ID}},
		{"/v1/orders?limit=1&offset=1", "u1", "", []string{second.ID}},
		{"/v1/partner/orders", "", token, []string{first.ID, third.ID}},
		{"/v1/partner/orders?status=rejected", "", token, []string{second.ID}},
		{"/v1/partner/orders", "", otherToken, nil},
	} {
		r := f.call("GET", tc.path, tc.user, "", tc.key, nil)
		f.expect(r, 200)
		var body struct {
			Items []domain.Order `json:"items"`
		}
		if err := json.Unmarshal(r.body, &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Items) != len(tc.ids) {
			t.Fatalf("%s: got %d items, want %d", tc.path, len(body.Items), len(tc.ids))
		}
		for i, o := range body.Items {
			if o.ID != tc.ids[i] {
				t.Fatalf("%s: wrong owner/order", tc.path)
			}
		}
	}
	if _, err := f.s.Order(t.Context(), first.ID, "", strings.ToUpper(f.p.ID)); err != nil {
		t.Fatalf("UUID comparison failed: %v", err)
	}
	for _, owners := range [][2]string{{"", ""}, {"u1", f.p.ID}} {
		if _, err := f.s.Order(t.Context(), first.ID, owners[0], owners[1]); err == nil {
			t.Fatal("ambiguous owner accepted")
		}
		if _, err := f.s.Orders(t.Context(), owners[0], owners[1], "", "", 20, 0); err == nil {
			t.Fatal("unscoped list accepted")
		}
	}
}

func TestIntegrationMigrationUpgradePreservesOrders(t *testing.T) {
	f := setup(t, 2)
	o := f.order(f.call("POST", "/v1/orders", "u1", "saved", "", f.input()), 201)
	if _, err := f.s.DB.ExecContext(t.Context(), `DROP INDEX orders_partner_history_idx; DROP INDEX orders_user_active_idx; DELETE FROM schema_migrations WHERE version=2`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrations.Run(t.Context(), f.s.DB, "up"); err != nil {
			t.Fatal(err)
		}
	}
	got := f.order(f.call("GET", "/v1/orders/"+o.ID, "u1", "", "", nil), 200)
	if got.ID != o.ID || f.stock() != 1 {
		t.Fatal("upgrade changed business data")
	}
	var versions int
	if err := f.s.DB.QueryRowContext(t.Context(), `SELECT count(*) FROM schema_migrations`).Scan(&versions); err != nil || versions != migrations.LatestVersion {
		t.Fatalf("versions=%d err=%v", versions, err)
	}
	if _, err := f.s.DB.ExecContext(t.Context(), `UPDATE schema_migrations SET checksum='changed' WHERE version=1`); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Run(t.Context(), f.s.DB, "up"); err == nil {
		t.Fatal("modified migration checksum accepted")
	}
	if err := f.s.CheckRuntimeRole(t.Context()); err == nil {
		t.Fatal("migration role accepted as runtime role")
	}
}
