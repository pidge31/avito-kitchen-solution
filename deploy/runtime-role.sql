\set ON_ERROR_STOP on
\getenv app_password APP_DATABASE_PASSWORD

BEGIN;
SELECT 'CREATE ROLE kitchen_app' WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'kitchen_app')
\gexec
SELECT format('ALTER ROLE kitchen_app WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD %L', :'app_password')
\gexec

REVOKE CREATE ON SCHEMA public FROM PUBLIC;
REVOKE ALL ON SCHEMA public FROM kitchen_app;
GRANT USAGE ON SCHEMA public TO kitchen_app;
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM kitchen_app;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM kitchen_app;

GRANT SELECT ON schema_migrations, restaurants, menu_items, inventory_adjustments,
    orders, order_items, order_events TO kitchen_app;
GRANT UPDATE (is_open) ON restaurants TO kitchen_app;
GRANT INSERT ON menu_items TO kitchen_app;
GRANT UPDATE (name, description, category, price_minor, active, available_stock) ON menu_items TO kitchen_app;
GRANT INSERT ON inventory_adjustments, orders, order_items, order_events TO kitchen_app;
GRANT UPDATE (status, updated_at, external_order_id) ON orders TO kitchen_app;
GRANT USAGE ON SEQUENCE order_events_id_seq TO kitchen_app;
COMMIT;
