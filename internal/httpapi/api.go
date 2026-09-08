package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"avito-kitchen/internal/domain"
	"avito-kitchen/internal/store"
	"avito-kitchen/openapi"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rs/zerolog/log"
)

type API struct {
	store             *store.Store
	ttl               time.Duration
	gatewayHash       [32]byte
	gatewayConfigured bool
}
type endpoint func(http.ResponseWriter, *http.Request) error
type partnerEndpoint func(http.ResponseWriter, *http.Request, string) error

func New(s *store.Store, ttl time.Duration, gatewayKey string) http.Handler {
	a := &API{store: s, ttl: ttl, gatewayHash: sha256.Sum256([]byte(gatewayKey)), gatewayConfigured: len(gatewayKey) >= 32 && len(gatewayKey) <= 512}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.wrap(a.health))
	mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		_, _ = w.Write(openapi.Spec)
	})
	mux.HandleFunc("GET /v1/restaurants", a.wrap(a.restaurants))
	mux.HandleFunc("GET /v1/restaurants/{restaurant_id}", a.wrap(a.restaurant))
	mux.HandleFunc("GET /v1/restaurants/{restaurant_id}/menu", a.wrap(a.menu))
	mux.HandleFunc("POST /v1/orders", a.wrap(a.createOrder))
	mux.HandleFunc("GET /v1/orders", a.wrap(a.orders))
	mux.HandleFunc("GET /v1/orders/{order_id}", a.wrap(a.order))
	mux.HandleFunc("POST /v1/orders/{order_id}/cancel", a.wrap(a.cancel))
	mux.HandleFunc("GET /v1/partner/restaurant", a.wrap(a.partner(a.partnerRestaurant)))
	mux.HandleFunc("PATCH /v1/partner/restaurant", a.wrap(a.partner(a.setOpen)))
	mux.HandleFunc("GET /v1/partner/menu", a.wrap(a.partner(a.partnerMenu)))
	mux.HandleFunc("PUT /v1/partner/menu/items/{external_id}", a.wrap(a.partner(a.putMenu)))
	mux.HandleFunc("POST /v1/partner/inventory-adjustments", a.wrap(a.partner(a.adjustInventory)))
	mux.HandleFunc("GET /v1/partner/orders", a.wrap(a.partner(a.partnerOrders)))
	mux.HandleFunc("GET /v1/partner/orders/{order_id}", a.wrap(a.partner(a.partnerOrder)))
	mux.HandleFunc("POST /v1/partner/orders/{order_id}/transitions", a.wrap(a.partner(a.transition)))
	return observe(newAdmission().wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		handler, pattern := mux.Handler(r)
		if pattern == "" {
			recorder := &routeResponse{header: make(http.Header)}
			handler.ServeHTTP(recorder, r)
			if allow := recorder.header.Get("Allow"); allow != "" {
				w.Header().Set("Allow", allow)
			}
			status := recorder.status
			if status == 0 {
				status = 404
			}
			write(w, status, map[string]any{"error": domain.Error{Code: "route_not_found", Message: http.StatusText(status)}})
			return
		}
		mux.ServeHTTP(w, r)
	})))
}

type routeResponse struct {
	header http.Header
	status int
}

func (r *routeResponse) Header() http.Header         { return r.header }
func (r *routeResponse) WriteHeader(s int)           { r.status = s }
func (r *routeResponse) Write(b []byte) (int, error) { return len(b), nil }

func (a *API) wrap(next endpoint) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if err := next(w, r.WithContext(ctx)); err != nil {
			var de *domain.Error
			var pe *pgconn.PgError
			switch {
			case errors.As(err, &de):
			case errors.As(err, &pe) && pe.Code == "23505":
				de = &domain.Error{Status: 409, Code: "conflict", Message: "external ID is already used"}
			case errors.Is(err, context.DeadlineExceeded):
				de = &domain.Error{Status: 503, Code: "timeout", Message: "request timed out; retry with the same idempotency key"}
			default:
				code := ""
				if pe != nil {
					code = pe.Code
				}
				log.Ctx(r.Context()).Error().Str("route", r.Pattern).Str("database_code", code).Str("error_type", errorType(err)).Msg("request failed")
				de = &domain.Error{Status: 500, Code: "internal_error", Message: "internal server error"}
			}
			write(w, de.Status, map[string]any{"error": de})
		}
	}
}

func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decode(w http.ResponseWriter, r *http.Request, v any) error {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return domain.Fail(415, "unsupported_media_type", "Content-Type must be application/json")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	defer func() { _ = r.Body.Close() }()
	d := json.NewDecoder(r.Body)
	var raw json.RawMessage
	if err = d.Decode(&raw); err != nil {
		return decodeError(err)
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		if err != nil {
			return decodeError(err)
		}
		return domain.Fail(400, "invalid_json", "exactly one JSON object is required")
	}
	tokens := json.NewDecoder(bytes.NewReader(raw))
	for {
		token, err := tokens.Token()
		if err == io.EOF {
			break
		}
		if err != nil || token == nil {
			return domain.Fail(400, "invalid_json", "null values are not allowed")
		}
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return decodeError(err)
	}
	return nil
}
func decodeError(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return domain.Fail(413, "body_too_large", "body exceeds 64 KiB")
	}
	return domain.Fail(400, "invalid_json", "invalid JSON body or unknown fields")
}
func (a *API) user(r *http.Request) (string, error) {
	key := sha256.Sum256([]byte(r.Header.Get("X-Gateway-Key")))
	if !a.gatewayConfigured || len(r.Header.Values("X-Gateway-Key")) != 1 || subtle.ConstantTimeCompare(key[:], a.gatewayHash[:]) != 1 {
		return "", domain.Fail(401, "invalid_gateway_key", "a trusted gateway key is required for customer orders")
	}
	u := r.Header.Get("X-User-ID")
	if len(r.Header.Values("X-User-ID")) != 1 || !domain.Key(u) {
		return "", domain.Fail(400, "user_id_required", "X-User-ID must be a valid external user identifier")
	}
	return u, nil
}
func pathID(r *http.Request, name string) (string, error) {
	id := r.PathValue(name)
	if !domain.UUID(id) {
		return "", domain.Fail(400, "invalid_id", "path identifier must be a UUID")
	}
	return id, nil
}
func page(r *http.Request) (int, int, error) {
	limit, offset := 20, 0
	for key, target := range map[string]*int{"limit": &limit, "offset": &offset} {
		if raw, ok := r.URL.Query()[key]; ok {
			if len(raw) != 1 {
				return 0, 0, domain.Fail(400, "invalid_pagination", "duplicate pagination parameter")
			}
			v, err := strconv.Atoi(raw[0])
			if err != nil {
				return 0, 0, domain.Fail(400, "invalid_pagination", "limit and offset must be integers")
			}
			*target = v
		}
	}
	if limit < 1 || limit > 100 || offset < 0 || offset > 100000 {
		return 0, 0, domain.Fail(400, "invalid_pagination", "limit must be 1..100 and offset 0..100000")
	}
	return limit, offset, nil
}
func statusFilter(r *http.Request, defaultStatus string) (string, error) {
	s := r.URL.Query().Get("status")
	if s == "" {
		s = defaultStatus
	}
	if s != "" && s != "active" && !domain.Status(s) {
		return "", domain.Fail(400, "invalid_status", "unknown order status")
	}
	return s, nil
}
func list(w http.ResponseWriter, v any, limit, offset int) {
	write(w, 200, map[string]any{"items": v, "limit": limit, "offset": offset})
}

func (a *API) health(w http.ResponseWriter, r *http.Request) error {
	if err := a.store.DB.PingContext(r.Context()); err != nil {
		return domain.Fail(503, "unavailable", "database is unavailable")
	}
	write(w, 200, map[string]string{"status": "ok"})
	return nil
}
func (a *API) restaurants(w http.ResponseWriter, r *http.Request) error {
	l, o, err := page(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	if !domain.Text(q.Get("city"), 0, 100) || !domain.Text(q.Get("q"), 0, 200) {
		return domain.Fail(400, "invalid_filter", "filter is too long")
	}
	items, err := a.store.Restaurants(r.Context(), q.Get("city"), q.Get("q"), l, o)
	if err != nil {
		return err
	}
	list(w, items, l, o)
	return nil
}
func (a *API) restaurant(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "restaurant_id")
	if err != nil {
		return err
	}
	v, err := a.store.Restaurant(r.Context(), id)
	if err != nil {
		return err
	}
	write(w, 200, v)
	return nil
}
func (a *API) menu(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "restaurant_id")
	if err != nil {
		return err
	}
	return a.menuFor(w, r, id, false)
}
func (a *API) menuFor(w http.ResponseWriter, r *http.Request, id string, partner bool) error {
	l, o, err := page(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	if !domain.Text(q.Get("q"), 0, 200) || !domain.Text(q.Get("category"), 0, 100) {
		return domain.Fail(400, "invalid_filter", "filter is too long")
	}
	items, err := a.store.Menu(r.Context(), id, q.Get("q"), q.Get("category"), partner, l, o)
	if err != nil {
		return err
	}
	list(w, items, l, o)
	return nil
}
func (a *API) createOrder(w http.ResponseWriter, r *http.Request) error {
	u, err := a.user(r)
	if err != nil {
		return err
	}
	var in domain.CreateOrder
	if err = decode(w, r, &in); err != nil {
		return err
	}
	v, replay, err := a.store.CreateOrder(r.Context(), u, r.Header.Get("Idempotency-Key"), in, a.ttl)
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/v1/orders/"+v.ID)
	w.Header().Set("Idempotency-Replayed", strconv.FormatBool(replay))
	code := 201
	if replay {
		code = 200
	}
	write(w, code, v)
	return nil
}
func (a *API) orders(w http.ResponseWriter, r *http.Request) error {
	u, err := a.user(r)
	if err != nil {
		return err
	}
	return a.ordersFor(w, r, u, "")
}
func (a *API) ordersFor(w http.ResponseWriter, r *http.Request, u, p string) error {
	l, o, err := page(r)
	if err != nil {
		return err
	}
	defaultStatus := ""
	if p != "" {
		defaultStatus = "active"
	}
	status, err := statusFilter(r, defaultStatus)
	if err != nil {
		return err
	}
	after := r.URL.Query().Get("after")
	if values, present := r.URL.Query()["after"]; present && (len(values) != 1 || after == "") {
		return domain.Fail(400, "invalid_cursor", "after must contain one order UUID")
	}
	items, err := a.store.Orders(r.Context(), u, p, status, after, l, o)
	if err != nil {
		return err
	}
	list(w, items, l, o)
	return nil
}
func (a *API) order(w http.ResponseWriter, r *http.Request) error {
	u, err := a.user(r)
	if err != nil {
		return err
	}
	return a.orderFor(w, r, u, "")
}
func (a *API) orderFor(w http.ResponseWriter, r *http.Request, u, p string) error {
	id, err := pathID(r, "order_id")
	if err != nil {
		return err
	}
	v, err := a.store.Order(r.Context(), id, u, p)
	if err != nil {
		return err
	}
	write(w, 200, v)
	return nil
}
func (a *API) cancel(w http.ResponseWriter, r *http.Request) error {
	u, err := a.user(r)
	if err != nil {
		return err
	}
	id, err := pathID(r, "order_id")
	if err != nil {
		return err
	}
	v, err := a.store.Cancel(r.Context(), id, u)
	if err != nil {
		return err
	}
	write(w, 200, v)
	return nil
}
func (a *API) partner(next partnerEndpoint) endpoint {
	return func(w http.ResponseWriter, r *http.Request) error {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || len(token) < 24 || len(token) > 512 {
			return domain.Fail(401, "invalid_api_key", "partner bearer API key is required")
		}
		p, err := a.store.Partner(r.Context(), token)
		if err != nil {
			return err
		}
		return next(w, r, p.ID)
	}
}
func (a *API) partnerRestaurant(w http.ResponseWriter, r *http.Request, p string) error {
	v, err := a.store.Restaurant(r.Context(), p)
	if err != nil {
		return err
	}
	write(w, 200, v)
	return nil
}
func (a *API) setOpen(w http.ResponseWriter, r *http.Request, p string) error {
	var in struct {
		IsOpen *bool `json:"is_open"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if in.IsOpen == nil {
		return domain.Fail(400, "is_open_required", "is_open is required")
	}
	v, err := a.store.SetOpen(r.Context(), p, *in.IsOpen)
	if err != nil {
		return err
	}
	write(w, 200, v)
	return nil
}
func (a *API) partnerMenu(w http.ResponseWriter, r *http.Request, p string) error {
	return a.menuFor(w, r, p, true)
}
func (a *API) putMenu(w http.ResponseWriter, r *http.Request, p string) error {
	var in domain.MenuInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	v, err := a.store.PutMenu(r.Context(), p, r.PathValue("external_id"), in)
	if err != nil {
		return err
	}
	write(w, 200, v)
	return nil
}
func (a *API) adjustInventory(w http.ResponseWriter, r *http.Request, p string) error {
	var in domain.Adjustment
	if err := decode(w, r, &in); err != nil {
		return err
	}
	v, replay, err := a.store.AdjustInventory(r.Context(), p, in)
	if err != nil {
		return err
	}
	w.Header().Set("Idempotency-Replayed", strconv.FormatBool(replay))
	write(w, 200, v)
	return nil
}
func (a *API) partnerOrders(w http.ResponseWriter, r *http.Request, p string) error {
	return a.ordersFor(w, r, "", p)
}
func (a *API) partnerOrder(w http.ResponseWriter, r *http.Request, p string) error {
	return a.orderFor(w, r, "", p)
}
func (a *API) transition(w http.ResponseWriter, r *http.Request, p string) error {
	id, err := pathID(r, "order_id")
	if err != nil {
		return err
	}
	var in domain.Transition
	if err = decode(w, r, &in); err != nil {
		return err
	}
	v, err := a.store.Transition(r.Context(), id, p, in)
	if err != nil {
		return err
	}
	write(w, 200, v)
	return nil
}
