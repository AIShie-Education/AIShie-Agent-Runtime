-- AIShie Agent Runtime, store migration 0010 (down)
-- Reverts 0010_site_prices.up.sql: the site's prices and tenants' quotas
-- go, and the price file and runtime.yaml decide alone again. The costs the
-- ledger recorded keep the versions they name.

BEGIN;

SET LOCAL lock_timeout = '10s';

DROP TABLE IF EXISTS site_tenant_quota;
DROP TABLE IF EXISTS site_price;
DROP FUNCTION IF EXISTS site_price_changed();
DROP TABLE IF EXISTS site_price_rev;

COMMIT;
