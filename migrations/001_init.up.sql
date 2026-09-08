CREATE TABLE restaurants (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    external_id text NOT NULL UNIQUE,
    name text NOT NULL,
    city text NOT NULL,
    cuisine text NOT NULL,
    enabled boolean NOT NULL DEFAULT true,
    is_open boolean NOT NULL DEFAULT false,
    api_key_hash text NOT NULL UNIQUE CHECK (length(api_key_hash) = 64),
    min_order_minor bigint NOT NULL DEFAULT 30000 CHECK (min_order_minor >= 0),
    delivery_fee_minor bigint NOT NULL DEFAULT 19900 CHECK (delivery_fee_minor >= 0),
    currency text NOT NULL DEFAULT 'RUB' CHECK (currency = 'RUB')
);
CREATE INDEX restaurants_city_idx ON restaurants (lower(city)) WHERE enabled;

CREATE TABLE menu_items (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    restaurant_id uuid NOT NULL REFERENCES restaurants(id),
    external_id text NOT NULL,
    name text NOT NULL,
    description text NOT NULL DEFAULT '',
    category text NOT NULL,
    price_minor bigint NOT NULL CHECK (price_minor BETWEEN 1 AND 100000000),
    available_stock bigint NOT NULL DEFAULT 0 CHECK (available_stock >= 0),
    active boolean NOT NULL DEFAULT true,
    UNIQUE (restaurant_id, external_id),
    UNIQUE (id, restaurant_id)
);

CREATE TABLE inventory_adjustments (
    restaurant_id uuid NOT NULL REFERENCES restaurants(id),
    external_id text NOT NULL,
    item_id uuid NOT NULL,
    delta integer NOT NULL CHECK (delta <> 0 AND delta BETWEEN -1000000 AND 1000000),
    request_hash text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (restaurant_id, external_id),
    FOREIGN KEY (item_id, restaurant_id) REFERENCES menu_items(id, restaurant_id)
);

CREATE TABLE orders (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    restaurant_id uuid NOT NULL REFERENCES restaurants(id),
    user_id text NOT NULL CHECK (length(user_id) BETWEEN 1 AND 128),
    idempotency_key text NOT NULL,
    request_hash text NOT NULL,
    external_order_id text,
    city text NOT NULL,
    delivery_address text NOT NULL,
    status text NOT NULL DEFAULT 'pending' CHECK (status IN
        ('pending','accepted','preparing','delivering','delivered','cancelled','rejected','expired')),
    subtotal_minor bigint NOT NULL CHECK (subtotal_minor > 0),
    delivery_fee_minor bigint NOT NULL CHECK (delivery_fee_minor >= 0),
    total_minor bigint GENERATED ALWAYS AS (subtotal_minor + delivery_fee_minor) STORED,
    currency text NOT NULL DEFAULT 'RUB' CHECK (currency = 'RUB'),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    CHECK (status NOT IN ('accepted','preparing','delivering','delivered') OR external_order_id IS NOT NULL),
    UNIQUE (user_id, idempotency_key),
    UNIQUE (restaurant_id, external_order_id),
    UNIQUE (id, restaurant_id)
);
CREATE INDEX orders_user_idx ON orders (user_id, created_at, id);
CREATE INDEX orders_partner_idx ON orders (restaurant_id, created_at, id)
    WHERE status IN ('pending','accepted','preparing','delivering');
CREATE INDEX orders_expiration_idx ON orders (expires_at) WHERE status = 'pending';

CREATE TABLE order_items (
    order_id uuid NOT NULL,
    restaurant_id uuid NOT NULL,
    item_id uuid NOT NULL,
    external_id text NOT NULL,
    name text NOT NULL,
    quantity integer NOT NULL CHECK (quantity BETWEEN 1 AND 99),
    price_minor bigint NOT NULL CHECK (price_minor BETWEEN 1 AND 100000000),
    line_total_minor bigint GENERATED ALWAYS AS (price_minor * quantity) STORED,
    PRIMARY KEY (order_id, item_id),
    FOREIGN KEY (order_id, restaurant_id) REFERENCES orders(id, restaurant_id),
    FOREIGN KEY (item_id, restaurant_id) REFERENCES menu_items(id, restaurant_id)
);

CREATE TABLE order_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id uuid NOT NULL REFERENCES orders(id),
    status text NOT NULL CHECK (status IN
        ('pending','accepted','preparing','delivering','delivered','cancelled','rejected','expired')),
    actor text NOT NULL CHECK (actor IN ('customer','partner','system')),
    reason text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX order_events_order_idx ON order_events (order_id, id);
