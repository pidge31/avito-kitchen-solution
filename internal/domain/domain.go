package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string                    { return e.Message }
func Fail(status int, code, message string) error { return &Error{status, code, message} }

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
var keyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:@-]{0,127}$`)

func UUID(s string) bool { return uuidPattern.MatchString(s) }
func Key(s string) bool  { return keyPattern.MatchString(s) }
func Text(s string, min, max int) bool {
	n := utf8.RuneCountInString(strings.TrimSpace(s))
	return n >= min && utf8.RuneCountInString(s) <= max && !strings.ContainsRune(s, 0)
}
func Hash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func TokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

type Restaurant struct {
	ID               string `json:"id"`
	ExternalID       string `json:"external_id"`
	Name             string `json:"name"`
	City             string `json:"city"`
	Cuisine          string `json:"cuisine"`
	IsOpen           bool   `json:"is_open"`
	MinOrderMinor    int64  `json:"min_order_minor"`
	DeliveryFeeMinor int64  `json:"delivery_fee_minor"`
	Currency         string `json:"currency"`
}

type MenuItem struct {
	ID             string `json:"id"`
	RestaurantID   string `json:"restaurant_id"`
	ExternalID     string `json:"external_id"`
	Name           string `json:"name"`
	Description    string `json:"description"`
	Category       string `json:"category"`
	PriceMinor     int64  `json:"price_minor"`
	AvailableStock int    `json:"available_stock"`
	Active         bool   `json:"active"`
}

type MenuInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Category    string `json:"category"`
	PriceMinor  int64  `json:"price_minor"`
	Active      *bool  `json:"active"`
}

func (m MenuInput) Validate() error {
	if !Text(m.Name, 1, 200) || !Text(m.Description, 0, 2000) || !Text(m.Category, 1, 100) || m.PriceMinor < 1 || m.PriceMinor > 100_000_000 || m.Active == nil {
		return Fail(400, "invalid_menu_item", "name, category, price_minor (1..100000000) and active are required")
	}
	return nil
}

type Adjustment struct {
	ExternalID     string `json:"external_id"`
	ItemExternalID string `json:"item_external_id"`
	Delta          int    `json:"delta"`
}

func (a Adjustment) Validate() error {
	if !Key(a.ExternalID) || !Key(a.ItemExternalID) || a.Delta == 0 || a.Delta < -1_000_000 || a.Delta > 1_000_000 {
		return Fail(400, "invalid_adjustment", "valid external IDs and nonzero delta (-1000000..1000000) are required")
	}
	return nil
}

type OrderLineInput struct {
	ItemID             string `json:"item_id"`
	Quantity           int    `json:"quantity"`
	ExpectedPriceMinor int64  `json:"expected_price_minor"`
}
type CreateOrder struct {
	RestaurantID    string           `json:"restaurant_id"`
	City            string           `json:"city"`
	DeliveryAddress string           `json:"delivery_address"`
	Items           []OrderLineInput `json:"items"`
}

func (o *CreateOrder) Normalize() error {
	o.RestaurantID = strings.ToLower(o.RestaurantID)
	o.City = strings.TrimSpace(o.City)
	o.DeliveryAddress = strings.TrimSpace(o.DeliveryAddress)
	if !UUID(o.RestaurantID) || !Text(o.City, 1, 100) || !Text(o.DeliveryAddress, 10, 500) || len(o.Items) < 1 || len(o.Items) > 50 {
		return Fail(400, "invalid_order", "restaurant_id, city, delivery_address (10..500 characters), and 1..50 items are required")
	}
	seen := make(map[string]bool, len(o.Items))
	for i := range o.Items {
		line := &o.Items[i]
		line.ItemID = strings.ToLower(line.ItemID)
		if !UUID(line.ItemID) || seen[line.ItemID] || line.Quantity < 1 || line.Quantity > 99 || line.ExpectedPriceMinor < 1 || line.ExpectedPriceMinor > 100_000_000 {
			return Fail(400, "invalid_order_item", "items must be unique, with quantity 1..99 and expected_price_minor 1..100000000")
		}
		seen[line.ItemID] = true
	}
	slices.SortFunc(o.Items, func(a, b OrderLineInput) int { return strings.Compare(a.ItemID, b.ItemID) })
	return nil
}

type OrderLine struct {
	ItemID         string `json:"item_id"`
	ExternalID     string `json:"external_id"`
	Name           string `json:"name"`
	Quantity       int    `json:"quantity"`
	PriceMinor     int64  `json:"price_minor"`
	LineTotalMinor int64  `json:"line_total_minor"`
}
type Event struct {
	Status    string    `json:"status"`
	Actor     string    `json:"actor"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}
type Order struct {
	ID               string      `json:"id"`
	RestaurantID     string      `json:"restaurant_id"`
	UserID           string      `json:"user_id"`
	ExternalOrderID  string      `json:"external_order_id"`
	City             string      `json:"city"`
	DeliveryAddress  string      `json:"delivery_address"`
	Status           string      `json:"status"`
	SubtotalMinor    int64       `json:"subtotal_minor"`
	DeliveryFeeMinor int64       `json:"delivery_fee_minor"`
	TotalMinor       int64       `json:"total_minor"`
	Currency         string      `json:"currency"`
	CreatedAt        time.Time   `json:"created_at"`
	UpdatedAt        time.Time   `json:"updated_at"`
	ExpiresAt        time.Time   `json:"expires_at"`
	Items            []OrderLine `json:"items"`
	Events           []Event     `json:"events"`
}

type Transition struct {
	FromStatus      string `json:"from_status"`
	ToStatus        string `json:"to_status"`
	ExternalOrderID string `json:"external_order_id,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

func Status(s string) bool {
	return slices.Contains([]string{"pending", "accepted", "preparing", "delivering", "delivered", "cancelled", "rejected", "expired"}, s)
}
func CanTransition(from, to string) bool {
	return (from == "pending" && (to == "accepted" || to == "rejected")) ||
		(from == "accepted" && to == "preparing") ||
		(from == "preparing" && to == "delivering") ||
		(from == "delivering" && to == "delivered")
}
func (t Transition) Validate() error {
	if !CanTransition(t.FromStatus, t.ToStatus) {
		return Fail(400, "invalid_transition", fmt.Sprintf("transition %s -> %s is not supported", t.FromStatus, t.ToStatus))
	}
	if t.ToStatus == "accepted" && !Key(t.ExternalOrderID) {
		return Fail(400, "external_order_id_required", "accepting an order requires a valid external_order_id")
	}
	if t.ToStatus != "accepted" && t.ExternalOrderID != "" {
		return Fail(400, "invalid_transition", "external_order_id can only be set when accepting")
	}
	if !Text(t.Reason, 0, 500) || (t.ToStatus == "rejected" && !Text(t.Reason, 1, 500)) {
		return Fail(400, "reason_required", "rejection requires a reason (1..500 characters)")
	}
	return nil
}
