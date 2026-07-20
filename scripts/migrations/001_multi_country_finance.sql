-- Additive upgrade. Applied once in the same transaction as its ledger entry.
ALTER TABLE stations ADD COLUMN country_code varchar(2) NOT NULL DEFAULT 'MY';
ALTER TABLE stations ADD COLUMN is_simulated boolean NOT NULL DEFAULT false;
ALTER TABLE tariffs ALTER COLUMN price_per_kwh TYPE numeric(12,6);
ALTER TABLE connectors ALTER COLUMN tariff_per_kwh TYPE numeric(12,6);
ALTER TABLE charging_sessions ALTER COLUMN tariff_per_kwh TYPE numeric(12,6);
ALTER TABLE tariffs ADD COLUMN pricing_supported boolean NOT NULL DEFAULT true;
ALTER TABLE tariffs ADD COLUMN ocpi_json jsonb;

CREATE TABLE exchange_rates (
  id varchar(64) PRIMARY KEY,
  source_currency varchar(3) NOT NULL CHECK (source_currency IN ('MYR','SGD','THB')),
  target_currency varchar(3) NOT NULL CHECK (target_currency IN ('MYR','SGD','THB')),
  rate numeric(20,10) NOT NULL CHECK (rate > 0),
  effective_at timestamptz NOT NULL,
  source varchar(128) NOT NULL,
  CHECK (source_currency <> target_currency OR rate = 1),
  UNIQUE (source_currency, target_currency, effective_at, source)
);
CREATE INDEX exchange_rates_latest ON exchange_rates(source_currency,target_currency,effective_at DESC,id DESC);
-- Fixed demonstration values, never market-rate claims. Add rows to change rates.
INSERT INTO exchange_rates VALUES
 ('fx_demo_sgd_myr','SGD','MYR',3.45,'2026-01-01T00:00:00Z','chargenet-demo-v1'),
 ('fx_demo_thb_myr','THB','MYR',0.13,'2026-01-01T00:00:00Z','chargenet-demo-v1'),
 ('fx_demo_myr_sgd','MYR','SGD',0.2898550725,'2026-01-01T00:00:00Z','chargenet-demo-v1'),
 ('fx_demo_myr_thb','MYR','THB',7.6923076923,'2026-01-01T00:00:00Z','chargenet-demo-v1'),
 ('fx_demo_sgd_thb','SGD','THB',26.5384615385,'2026-01-01T00:00:00Z','chargenet-demo-v1'),
 ('fx_demo_thb_sgd','THB','SGD',0.0376811594,'2026-01-01T00:00:00Z','chargenet-demo-v1');

CREATE TABLE charging_quotes (
 id varchar(64) PRIMARY KEY,
 user_id varchar(64) NOT NULL REFERENCES users(id),
 wallet_id varchar(64) NOT NULL REFERENCES wallets(id),
 cpo_id varchar(64) NOT NULL REFERENCES cpo_partners(id),
 station_id varchar(64) NOT NULL REFERENCES stations(id),
 connector_id varchar(64) NOT NULL REFERENCES connectors(id),
 tariff_id varchar(64) NOT NULL REFERENCES tariffs(id),
 tariff_updated_at timestamptz NOT NULL,
 tariff_per_kwh numeric(12,6) NOT NULL CHECK (tariff_per_kwh > 0),
 original_currency varchar(3) NOT NULL CHECK (original_currency IN ('MYR','SGD','THB')),
 wallet_currency varchar(3) NOT NULL CHECK (wallet_currency IN ('MYR','SGD','THB')),
 exchange_rate_id varchar(64) REFERENCES exchange_rates(id),
 exchange_rate numeric(20,10) NOT NULL CHECK (exchange_rate > 0),
 rate_source varchar(128) NOT NULL,
 estimated_kwh numeric(8,3) NOT NULL CHECK (estimated_kwh > 0),
 estimated_original_cost numeric(12,2) NOT NULL,
 estimated_wallet_cost numeric(12,2) NOT NULL,
 budget numeric(12,2) NOT NULL CHECK (budget > 0),
 created_at timestamptz NOT NULL DEFAULT now(),
 expires_at timestamptz NOT NULL,
 accepted_at timestamptz,
 CHECK (expires_at > created_at),
 CHECK (original_currency <> wallet_currency OR exchange_rate = 1)
);
CREATE INDEX charging_quotes_user ON charging_quotes(user_id,created_at DESC);
CREATE INDEX charging_quotes_connector ON charging_quotes(connector_id);
CREATE INDEX charging_quotes_tariff ON charging_quotes(tariff_id);
CREATE INDEX charging_quotes_wallet ON charging_quotes(wallet_id);
CREATE INDEX charging_quotes_rate ON charging_quotes(exchange_rate_id);
CREATE INDEX charging_quotes_station ON charging_quotes(station_id);
CREATE INDEX charging_quotes_cpo ON charging_quotes(cpo_id);

ALTER TABLE charging_sessions ADD COLUMN quote_id varchar(64) UNIQUE REFERENCES charging_quotes(id);
ALTER TABLE charging_sessions ADD COLUMN original_currency varchar(3) NOT NULL DEFAULT 'MYR';
ALTER TABLE charging_sessions ADD COLUMN wallet_currency varchar(3) NOT NULL DEFAULT 'MYR';
ALTER TABLE charging_sessions ADD COLUMN exchange_rate numeric(20,10) NOT NULL DEFAULT 1 CHECK(exchange_rate > 0);
ALTER TABLE charging_sessions ADD COLUMN rate_source varchar(128) NOT NULL DEFAULT 'legacy-same-currency';
ALTER TABLE charging_sessions ADD COLUMN original_cost numeric(12,2) NOT NULL DEFAULT 0;
ALTER TABLE charging_sessions ADD COLUMN unfunded_amount numeric(12,2) NOT NULL DEFAULT 0 CHECK(unfunded_amount >= 0);
UPDATE charging_sessions SET original_cost = total_cost;

CREATE TABLE wallet_reservations (
 session_id varchar(64) PRIMARY KEY REFERENCES charging_sessions(id),
 wallet_id varchar(64) NOT NULL REFERENCES wallets(id),
 amount numeric(12,2) NOT NULL CHECK (amount > 0),
 status varchar(16) NOT NULL DEFAULT 'HELD' CHECK(status IN ('HELD','RELEASED','SETTLED')),
 created_at timestamptz NOT NULL DEFAULT now(),
 closed_at timestamptz
);
CREATE INDEX wallet_reservations_held ON wallet_reservations(wallet_id) WHERE status='HELD';
ALTER TABLE wallet_transactions ADD COLUMN session_id varchar(64) REFERENCES charging_sessions(id);
ALTER TABLE wallet_transactions ADD COLUMN cdr_id varchar(64) REFERENCES cdrs(id);
ALTER TABLE wallet_transactions ADD COLUMN currency varchar(3) NOT NULL DEFAULT 'MYR';
UPDATE wallet_transactions w SET session_id=c.session_id,cdr_id=c.id,currency=c.currency
 FROM cdrs c WHERE w.type='CHARGING_DEBIT' AND w.reference_id=c.id;
CREATE UNIQUE INDEX wallet_transactions_one_settlement ON wallet_transactions(session_id) WHERE type='CHARGING_DEBIT';
CREATE INDEX wallet_transactions_cdr ON wallet_transactions(cdr_id);
CREATE INDEX wallet_transactions_wallet_time ON wallet_transactions(wallet_id,created_at DESC,id DESC);
-- cdrs.session_id is the authoritative relationship; retain legacy cdr_id only
-- for old binaries. Reads join by session_id and new writes do not need it.
ALTER TABLE cdrs ADD COLUMN source_cdr_json jsonb;
ALTER TABLE cdrs ADD COLUMN ocpi_version varchar(8) NOT NULL DEFAULT '2.3.0';
CREATE INDEX cdrs_cpo_time ON cdrs(cpo_id,created_at DESC,id DESC);
CREATE INDEX cdrs_user ON cdrs(user_id);
CREATE INDEX sessions_station ON charging_sessions(station_id);
CREATE INDEX sessions_connector ON charging_sessions(connector_id);
CREATE INDEX sessions_cpo ON charging_sessions(cpo_id);
CREATE INDEX sessions_vehicle ON charging_sessions(vehicle_id);
CREATE UNIQUE INDEX sessions_ocpi_identity ON charging_sessions(cpo_id,ocpi_session_id) WHERE ocpi_session_id IS NOT NULL;
CREATE INDEX connectors_tariff ON connectors(tariff_id);
CREATE INDEX connectors_station ON connectors(station_id);
CREATE INDEX stations_country ON stations(country_code,cpo_id);
CREATE INDEX tariffs_cpo ON tariffs(cpo_id);
CREATE INDEX vehicles_user ON vehicles(user_id);
ALTER TABLE stations ADD CONSTRAINT stations_country CHECK(country_code IN ('MY','SG','TH')) NOT VALID;
ALTER TABLE stations ADD CONSTRAINT stations_coordinates CHECK(latitude BETWEEN -90 AND 90 AND longitude BETWEEN -180 AND 180) NOT VALID;
ALTER TABLE tariffs ADD CONSTRAINT tariffs_currency CHECK(currency IN ('MYR','SGD','THB')) NOT VALID;
ALTER TABLE wallets ADD CONSTRAINT wallets_currency CHECK(currency IN ('MYR','SGD','THB')) NOT VALID;
ALTER TABLE cdrs ADD CONSTRAINT cdrs_currency CHECK(currency IN ('MYR','SGD','THB')) NOT VALID;
ALTER TABLE charging_sessions ADD CONSTRAINT sessions_currencies CHECK(original_currency IN ('MYR','SGD','THB') AND wallet_currency IN ('MYR','SGD','THB')) NOT VALID;

CREATE FUNCTION protect_quote_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' OR (to_jsonb(NEW)-'accepted_at') IS DISTINCT FROM (to_jsonb(OLD)-'accepted_at')
    OR OLD.accepted_at IS NOT NULL THEN
   RAISE EXCEPTION 'charging quotation snapshots are immutable';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER charging_quote_immutable BEFORE UPDATE OR DELETE ON charging_quotes
 FOR EACH ROW EXECUTE FUNCTION protect_quote_snapshot();
CREATE FUNCTION protect_rate_version() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'exchange rates are append only'; END $$;
CREATE TRIGGER exchange_rate_immutable BEFORE UPDATE OR DELETE ON exchange_rates
 FOR EACH ROW EXECUTE FUNCTION protect_rate_version();
CREATE FUNCTION protect_session_pricing() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.quote_id IS NOT NULL AND
   ROW(NEW.quote_id,NEW.tariff_per_kwh,NEW.original_currency,NEW.wallet_currency,NEW.exchange_rate,NEW.rate_source)
   IS DISTINCT FROM ROW(OLD.quote_id,OLD.tariff_per_kwh,OLD.original_currency,OLD.wallet_currency,OLD.exchange_rate,OLD.rate_source)
 THEN RAISE EXCEPTION 'accepted session pricing is immutable'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER session_pricing_immutable BEFORE UPDATE ON charging_sessions
 FOR EACH ROW EXECUTE FUNCTION protect_session_pricing();

