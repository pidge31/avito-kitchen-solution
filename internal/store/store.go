package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"avito-kitchen/internal/domain"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type Store struct{ DB *sql.DB }

func Open(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err = db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{DB: db}, nil
}

type scanner interface{ Scan(...any) error }

// Запуск HTTP API с правами владельца миграций считается ошибкой конфигурации.
func (s *Store) CheckRuntimeRole(ctx context.Context) error {
	var unsafe bool
	err := s.DB.QueryRowContext(ctx, `SELECT rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls
		OR has_schema_privilege(current_schema(), 'CREATE') OR has_table_privilege('orders', 'TRUNCATE')
		FROM pg_roles WHERE rolname=current_user`).Scan(&unsafe)
	if err != nil {
		return fmt.Errorf("cannot verify database runtime privileges")
	}
	if unsafe {
		return fmt.Errorf("API database role is too privileged; use the restricted runtime role")
	}
	return nil
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Fail(404, "not_found", "resource not found")
	}
	return err
}

const restaurantColumns = `id,external_id,name,city,cuisine,is_open,min_order_minor,delivery_fee_minor,currency`

func restaurant(row scanner) (domain.Restaurant, error) {
	var r domain.Restaurant
	err := row.Scan(&r.ID, &r.ExternalID, &r.Name, &r.City, &r.Cuisine, &r.IsOpen, &r.MinOrderMinor, &r.DeliveryFeeMinor, &r.Currency)
	return r, notFound(err)
}

// Provision доступен оператору только через CLI и намеренно отсутствует в HTTP API.
func (s *Store) Provision(ctx context.Context, externalID, name, city, cuisine, token string) (domain.Restaurant, error) {
	if !domain.Key(externalID) || !domain.Text(name, 1, 200) || !domain.Text(city, 1, 100) || !domain.Text(cuisine, 1, 100) || len(token) < 24 || len(token) > 512 {
		return domain.Restaurant{}, domain.Fail(400, "invalid_provisioning", "valid venue details and a 24..512-byte API key are required")
	}
	return restaurant(s.DB.QueryRowContext(ctx, `INSERT INTO restaurants(external_id,name,city,cuisine,api_key_hash)
		VALUES($1,$2,$3,$4,$5) ON CONFLICT(external_id) DO UPDATE SET
		name=excluded.name,city=excluded.city,cuisine=excluded.cuisine,api_key_hash=excluded.api_key_hash
		RETURNING `+restaurantColumns, externalID, name, city, cuisine, domain.TokenHash(token)))
}

func (s *Store) Partner(ctx context.Context, token string) (domain.Restaurant, error) {
	r, err := restaurant(s.DB.QueryRowContext(ctx, `SELECT `+restaurantColumns+` FROM restaurants WHERE api_key_hash=$1 AND enabled`, domain.TokenHash(token)))
	var de *domain.Error
	if errors.As(err, &de) && de.Status == 404 {
		return r, domain.Fail(401, "invalid_api_key", "invalid partner API key")
	}
	return r, err
}

func (s *Store) Restaurants(ctx context.Context, city, q string, limit, offset int) ([]domain.Restaurant, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+restaurantColumns+` FROM restaurants
		WHERE enabled AND ($1='' OR lower(city)=lower($1))
		AND ($2='' OR strpos(lower(name || ' ' || cuisine),lower($2))>0)
		ORDER BY name,id LIMIT $3 OFFSET $4`, city, q, limit, offset)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []domain.Restaurant{}
	for rows.Next() {
		r, e := restaurant(rows)
		if e != nil {
			return nil, e
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

func (s *Store) Restaurant(ctx context.Context, id string) (domain.Restaurant, error) {
	return restaurant(s.DB.QueryRowContext(ctx, `SELECT `+restaurantColumns+` FROM restaurants WHERE id=$1 AND enabled`, id))
}

func (s *Store) SetOpen(ctx context.Context, id string, open bool) (domain.Restaurant, error) {
	return restaurant(s.DB.QueryRowContext(ctx, `UPDATE restaurants SET is_open=$2 WHERE id=$1 AND enabled RETURNING `+restaurantColumns, id, open))
}

const itemColumns = `id,restaurant_id,external_id,name,description,category,price_minor,available_stock,active`

func item(row scanner) (domain.MenuItem, error) {
	var m domain.MenuItem
	err := row.Scan(&m.ID, &m.RestaurantID, &m.ExternalID, &m.Name, &m.Description, &m.Category, &m.PriceMinor, &m.AvailableStock, &m.Active)
	return m, notFound(err)
}

func (s *Store) Menu(ctx context.Context, id, q, category string, partner bool, limit, offset int) ([]domain.MenuItem, error) {
	if _, err := s.Restaurant(ctx, id); err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+itemColumns+` FROM menu_items WHERE restaurant_id=$1
		AND ($2 OR active) AND ($3='' OR strpos(lower(name || ' ' || description),lower($3))>0)
		AND ($4='' OR lower(category)=lower($4)) ORDER BY category,name,id LIMIT $5 OFFSET $6`, id, partner, q, category, limit, offset)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []domain.MenuItem{}
	for rows.Next() {
		m, e := item(rows)
		if e != nil {
			return nil, e
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

func (s *Store) PutMenu(ctx context.Context, id, externalID string, in domain.MenuInput) (domain.MenuItem, error) {
	if !domain.Key(externalID) {
		return domain.MenuItem{}, domain.Fail(400, "invalid_external_id", "invalid external_id")
	}
	if err := in.Validate(); err != nil {
		return domain.MenuItem{}, err
	}
	return item(s.DB.QueryRowContext(ctx, `INSERT INTO menu_items(restaurant_id,external_id,name,description,category,price_minor,active)
		VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(restaurant_id,external_id) DO UPDATE SET
		name=excluded.name,description=excluded.description,category=excluded.category,price_minor=excluded.price_minor,active=excluded.active
		RETURNING `+itemColumns, id, externalID, in.Name, in.Description, in.Category, in.PriceMinor, *in.Active))
}

// AdjustInventory применяет событие POS ровно один раз, а при повторе возвращает текущую позицию.
func (s *Store) AdjustInventory(ctx context.Context, id string, in domain.Adjustment) (domain.MenuItem, bool, error) {
	if err := in.Validate(); err != nil {
		return domain.MenuItem{}, false, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return domain.MenuItem{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "inventory:"+id+":"+in.ExternalID); err != nil {
		return domain.MenuItem{}, false, err
	}
	var hash string
	err = tx.QueryRowContext(ctx, `SELECT request_hash FROM inventory_adjustments WHERE restaurant_id=$1 AND external_id=$2`, id, in.ExternalID).Scan(&hash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return domain.MenuItem{}, false, err
	}
	replay := err == nil
	if replay && hash != domain.Hash(in) {
		return domain.MenuItem{}, false, domain.Fail(409, "idempotency_conflict", "inventory event ID was used with a different payload")
	}
	m, err := item(tx.QueryRowContext(ctx, `SELECT `+itemColumns+` FROM menu_items WHERE restaurant_id=$1 AND external_id=$2 FOR UPDATE`, id, in.ItemExternalID))
	if err != nil {
		return m, false, err
	}
	if !replay {
		if m.AvailableStock+in.Delta < 0 {
			return m, false, domain.Fail(409, "insufficient_stock", "adjustment would make available stock negative")
		}
		m, err = item(tx.QueryRowContext(ctx, `UPDATE menu_items SET available_stock=available_stock+$2 WHERE id=$1 RETURNING `+itemColumns, m.ID, in.Delta))
		if err != nil {
			return m, false, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO inventory_adjustments(restaurant_id,external_id,item_id,delta,request_hash) VALUES($1,$2,$3,$4,$5)`, id, in.ExternalID, m.ID, in.Delta, domain.Hash(in))
		if err != nil {
			return m, false, err
		}
	}
	return m, replay, tx.Commit()
}
