package domain

import "testing"

func TestOrderNormalization(t *testing.T) {
	a := CreateOrder{RestaurantID: "AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA", City: " Москва ", DeliveryAddress: " ул. Лесная, дом 12 ", Items: []OrderLineInput{
		{ItemID: "BBBBBBBB-BBBB-BBBB-BBBB-BBBBBBBBBBBB", Quantity: 1, ExpectedPriceMinor: 100},
		{ItemID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", Quantity: 2, ExpectedPriceMinor: 200},
	}}
	if err := a.Normalize(); err != nil {
		t.Fatal(err)
	}
	b := CreateOrder{RestaurantID: a.RestaurantID, City: a.City, DeliveryAddress: a.DeliveryAddress, Items: []OrderLineInput{a.Items[1], a.Items[0]}}
	if err := b.Normalize(); err != nil {
		t.Fatal(err)
	}
	if Hash(a) != Hash(b) {
		t.Fatal("basket order must not change idempotency hash")
	}
	b.Items = append(b.Items, b.Items[0])
	if b.Normalize() == nil {
		t.Fatal("duplicate item accepted")
	}
}

func TestStateMachine(t *testing.T) {
	allowed := map[[2]string]bool{{"pending", "accepted"}: true, {"pending", "rejected"}: true, {"accepted", "preparing"}: true, {"preparing", "delivering"}: true, {"delivering", "delivered"}: true}
	states := []string{"pending", "accepted", "preparing", "delivering", "delivered", "cancelled", "rejected", "expired", "unknown"}
	for _, from := range states {
		for _, to := range states {
			if CanTransition(from, to) != allowed[[2]string{from, to}] {
				t.Errorf("wrong transition %s -> %s", from, to)
			}
		}
	}
	for _, tr := range []Transition{{FromStatus: "pending", ToStatus: "accepted"}, {FromStatus: "pending", ToStatus: "rejected"}, {FromStatus: "accepted", ToStatus: "preparing", ExternalOrderID: "unexpected"}} {
		if tr.Validate() == nil {
			t.Errorf("invalid transition payload accepted: %+v", tr)
		}
	}
}
