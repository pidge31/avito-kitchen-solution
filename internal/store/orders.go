package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"avito-kitchen/internal/domain"
)

// Один SQL-запрос даёт согласованный снимок заголовка, позиций и истории заказа.
const orderSelect = `SELECT (to_jsonb(o) - 'idempotency_key' - 'request_hash') || jsonb_build_object(
	'external_order_id', coalesce(o.external_order_id,''),
	'items',coalesce((SELECT jsonb_agg(to_jsonb(i)-'order_id'-'restaurant_id' ORDER BY i.item_id)
		FROM order_items i WHERE i.order_id=o.id),'[]'::jsonb),
	'events',coalesce((SELECT jsonb_agg(to_jsonb(e)-'id'-'order_id' ORDER BY e.id)
		FROM order_events e WHERE e.order_id=o.id),'[]'::jsonb)) FROM orders o `

func order(row scanner) (domain.Order, error) {
	var o domain.Order
	var raw []byte
	if err := row.Scan(&raw); err != nil {
		return o, notFound(err)
	}
	err := json.Unmarshal(raw, &o)
	return o, err
}

func (s *Store) Order(ctx context.Context, id, user, partner string) (domain.Order, error) {
	column, identity, err := orderScope(user, partner)
	if err != nil {
		return domain.Order{}, err
	}
	return order(s.DB.QueryRowContext(ctx, orderSelect+`WHERE o.id=$1 AND `+column+`=$2`, id, identity))
}

func orderScope(user, partner string) (string, string, error) {
	if domain.Key(user) && partner == "" {
		return "o.user_id", user, nil
	}
	if user == "" && domain.UUID(partner) {
		return "o.restaurant_id", partner, nil
	}
	return "", "", domain.Fail(400, "invalid_identity", "exactly one valid order owner is required")
}

func (s *Store) Orders(ctx context.Context, user, partner, status, after string, limit, offset int) ([]domain.Order, error) {
	column, identity, err := orderScope(user, partner)
	if err != nil {
		return nil, err
	}
	query := orderSelect + `WHERE ` + column + `=$1`
	args := []any{identity}
	if after != "" && (partner == "" || !domain.UUID(after) || offset != 0) {
		return nil, domain.Fail(400, "invalid_cursor", "after requires a partner order UUID and zero offset")
	}
	switch {
	case status == "active":
		query += ` AND o.status IN ('pending','accepted','preparing','delivering')`
	case status == "":
	case domain.Status(status):
		query += ` AND o.status=$2`
		args = append(args, status)
	default:
		return nil, domain.Fail(400, "invalid_status", "unknown order status")
	}
	if after != "" {
		args = append(args, after)
		query += fmt.Sprintf(` AND (o.created_at,o.id) > (SELECT created_at,id FROM orders WHERE id=$%d AND restaurant_id=$1)`, len(args))
	}
	query += fmt.Sprintf(` ORDER BY o.created_at,o.id LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2)
	args = append(args, limit, offset)
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []domain.Order{}
	for rows.Next() {
		o, e := order(rows)
		if e != nil {
			return nil, e
		}
		result = append(result, o)
	}
	return result, rows.Err()
}

const MaxActiveOrdersPerUser = 5

func (s *Store) CreateOrder(ctx context.Context, user, key string, in domain.CreateOrder, ttl time.Duration) (domain.Order, bool, error) {
	var zero domain.Order
	if !domain.Key(user) || !domain.Key(key) {
		return zero, false, domain.Fail(400, "invalid_identity", "valid X-User-ID and Idempotency-Key are required")
	}
	if err := in.Normalize(); err != nil {
		return zero, false, err
	}
	hash := domain.Hash(in)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return zero, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "checkout-user:"+user); err != nil {
		return zero, false, err
	}
	var id, previousHash string
	err = tx.QueryRowContext(ctx, `SELECT id,request_hash FROM orders WHERE user_id=$1 AND idempotency_key=$2`, user, key).Scan(&id, &previousHash)
	if err == nil {
		if previousHash != hash {
			return zero, false, domain.Fail(409, "idempotency_conflict", "key was used with a different order")
		}
		o, e := order(tx.QueryRowContext(ctx, orderSelect+`WHERE o.id=$1`, id))
		if e != nil {
			return zero, false, e
		}
		return o, true, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return zero, false, err
	}
	var active int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM orders WHERE user_id=$1 AND status IN ('pending','accepted','preparing','delivering')`, user).Scan(&active); err != nil {
		return zero, false, err
	}
	if active >= MaxActiveOrdersPerUser {
		return zero, false, domain.Fail(409, "active_order_limit", "at most 5 active orders per user; complete or cancel an order first")
	}
	r, err := restaurant(tx.QueryRowContext(ctx, `SELECT `+restaurantColumns+` FROM restaurants WHERE id=$1 AND enabled FOR SHARE`, in.RestaurantID))
	if err != nil {
		return zero, false, err
	}
	if !r.IsOpen {
		return zero, false, domain.Fail(409, "restaurant_closed", "restaurant is closed")
	}
	if !strings.EqualFold(r.City, in.City) {
		return zero, false, domain.Fail(409, "outside_delivery_area", "delivery is available only within the restaurant's city")
	}
	var subtotal int64
	lines := make([]domain.OrderLine, 0, len(in.Items))
	for _, requested := range in.Items {
		m, e := item(tx.QueryRowContext(ctx, `SELECT `+itemColumns+` FROM menu_items WHERE id=$1 AND restaurant_id=$2 FOR UPDATE`, requested.ItemID, r.ID))
		if e != nil {
			var de *domain.Error
			if errors.As(e, &de) && de.Status == 404 {
				return zero, false, domain.Fail(409, "item_unavailable", "item does not belong to this restaurant")
			}
			return zero, false, e
		}
		if !m.Active || m.AvailableStock < requested.Quantity {
			return zero, false, domain.Fail(409, "item_unavailable", "item "+m.ID+" is unavailable in the requested quantity")
		}
		if m.PriceMinor != requested.ExpectedPriceMinor {
			return zero, false, domain.Fail(409, "price_changed", "price changed for item "+m.ID+"; refresh the menu and confirm")
		}
		line := domain.OrderLine{ItemID: m.ID, ExternalID: m.ExternalID, Name: m.Name, Quantity: requested.Quantity, PriceMinor: m.PriceMinor, LineTotalMinor: m.PriceMinor * int64(requested.Quantity)}
		lines = append(lines, line)
		subtotal += line.LineTotalMinor
	}
	if subtotal < r.MinOrderMinor {
		return zero, false, domain.Fail(409, "minimum_order", "subtotal is below the restaurant's minimum order")
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO orders(restaurant_id,user_id,idempotency_key,request_hash,city,delivery_address,subtotal_minor,delivery_fee_minor,expires_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,clock_timestamp()+($9::bigint*interval '1 millisecond')) RETURNING id`, r.ID, user, key, hash, r.City, in.DeliveryAddress, subtotal, r.DeliveryFeeMinor, ttl.Milliseconds()).Scan(&id)
	if err != nil {
		return zero, false, err
	}
	for _, line := range lines {
		_, err = tx.ExecContext(ctx, `INSERT INTO order_items(order_id,restaurant_id,item_id,external_id,name,quantity,price_minor) VALUES($1,$2,$3,$4,$5,$6,$7)`, id, r.ID, line.ItemID, line.ExternalID, line.Name, line.Quantity, line.PriceMinor)
		if err != nil {
			return zero, false, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE menu_items SET available_stock=available_stock-$2 WHERE id=$1`, line.ItemID, line.Quantity); err != nil {
			return zero, false, err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO order_events(order_id,status,actor) VALUES($1,'pending','customer')`, id); err != nil {
		return zero, false, err
	}
	o, err := order(tx.QueryRowContext(ctx, orderSelect+`WHERE o.id=$1`, id))
	if err != nil {
		return zero, false, err
	}
	return o, false, tx.Commit()
}

// restore блокирует позиции в том же порядке, что и оформление. Заказ к этому моменту уже заблокирован
func restore(ctx context.Context, tx *sql.Tx, id string) error {
	rows, err := tx.QueryContext(ctx, `SELECT item_id,quantity FROM order_items WHERE order_id=$1 ORDER BY item_id`, id)
	if err != nil {
		return err
	}
	type reservation struct {
		id  string
		qty int
	}
	items := []reservation{}
	for rows.Next() {
		var r reservation
		if err = rows.Scan(&r.id, &r.qty); err != nil {
			_ = rows.Close()
			return err
		}
		items = append(items, r)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, r := range items {
		if _, err = tx.ExecContext(ctx, `UPDATE menu_items SET available_stock=available_stock+$2 WHERE id=$1`, r.id, r.qty); err != nil {
			return err
		}
	}
	return nil
}

func setStatus(ctx context.Context, tx *sql.Tx, id, status, actor, reason, external string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE orders SET status=$2,updated_at=clock_timestamp(),external_order_id=coalesce(nullif($3,''),external_order_id) WHERE id=$1`, id, status, external); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO order_events(order_id,status,actor,reason) VALUES($1,$2,$3,$4)`, id, status, actor, reason)
	return err
}

func (s *Store) Cancel(ctx context.Context, id, user string) (domain.Order, error) {
	var zero domain.Order
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer func() { _ = tx.Rollback() }()
	var status string
	if err = tx.QueryRowContext(ctx, `SELECT status FROM orders WHERE id=$1 AND user_id=$2 FOR UPDATE`, id, user).Scan(&status); err != nil {
		return zero, notFound(err)
	}
	if status != "cancelled" {
		if status != "pending" {
			return zero, domain.Fail(409, "invalid_order_state", "only pending orders may be cancelled")
		}
		if err = restore(ctx, tx, id); err != nil {
			return zero, err
		}
		if err = setStatus(ctx, tx, id, "cancelled", "customer", "cancelled by customer", ""); err != nil {
			return zero, err
		}
	}
	o, err := order(tx.QueryRowContext(ctx, orderSelect+`WHERE o.id=$1`, id))
	if err != nil {
		return zero, err
	}
	return o, tx.Commit()
}

func (s *Store) Transition(ctx context.Context, id, partner string, in domain.Transition) (domain.Order, error) {
	var zero domain.Order
	if err := in.Validate(); err != nil {
		return zero, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer func() { _ = tx.Rollback() }()
	var status, external string
	if err = tx.QueryRowContext(ctx, `SELECT status,coalesce(external_order_id,'') FROM orders WHERE id=$1 AND restaurant_id=$2 FOR UPDATE`, id, partner).Scan(&status, &external); err != nil {
		return zero, notFound(err)
	}
	if status == in.ToStatus {
		var actor, reason string
		if err = tx.QueryRowContext(ctx, `SELECT actor,reason FROM order_events WHERE order_id=$1 ORDER BY id DESC LIMIT 1`, id).Scan(&actor, &reason); err != nil {
			return zero, err
		}
		if actor != "partner" || reason != in.Reason || (in.ToStatus == "accepted" && external != in.ExternalOrderID) {
			return zero, domain.Fail(409, "transition_conflict", "transition retry has a different payload")
		}
	} else {
		if status != in.FromStatus {
			return zero, domain.Fail(409, "invalid_order_state", "order status has changed; fetch the order again")
		}
		if status == "pending" {
			var expired bool
			if err = tx.QueryRowContext(ctx, `SELECT expires_at<=clock_timestamp() FROM orders WHERE id=$1`, id).Scan(&expired); err != nil {
				return zero, err
			}
			if expired {
				return zero, domain.Fail(409, "order_expired", "acceptance deadline has passed")
			}
		}
		if in.ToStatus == "rejected" {
			if err = restore(ctx, tx, id); err != nil {
				return zero, err
			}
		}
		if err = setStatus(ctx, tx, id, in.ToStatus, "partner", in.Reason, in.ExternalOrderID); err != nil {
			return zero, err
		}
	}
	o, err := order(tx.QueryRowContext(ctx, orderSelect+`WHERE o.id=$1`, id))
	if err != nil {
		return zero, err
	}
	return o, tx.Commit()
}

func (s *Store) ExpireOne(ctx context.Context) (bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM orders WHERE status='pending' AND expires_at<=clock_timestamp() ORDER BY expires_at,id LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err = restore(ctx, tx, id); err != nil {
		return false, err
	}
	if err = setStatus(ctx, tx, id, "expired", "system", "restaurant did not respond before the deadline", ""); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
