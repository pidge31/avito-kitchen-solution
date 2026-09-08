CREATE INDEX orders_user_active_idx ON orders (user_id)
    WHERE status IN ('pending','accepted','preparing','delivering');
CREATE INDEX orders_partner_history_idx ON orders (restaurant_id, created_at, id);
