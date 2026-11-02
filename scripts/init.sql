-- ============================================================================
-- ChargeNet backend schema.
-- Idempotent: safe to run on every boot (store.Migrate executes this file).
-- Contains schema + non-user seed data only. Users/portal-users are seeded
-- from Go (internal/store/seed_users.go, seed_portal.go) because they need
-- bcrypt hashes.
-- ============================================================================

-- ===================== CPO partners =====================
CREATE TABLE IF NOT EXISTS cpo_partners (
  id VARCHAR(64) PRIMARY KEY,
  name VARCHAR(128) NOT NULL,
  slug VARCHAR(32) UNIQUE NOT NULL,
  country_code VARCHAR(2) NOT NULL DEFAULT 'MY',
  party_id VARCHAR(3) NOT NULL,
  base_url TEXT NOT NULL,
  color_hex VARCHAR(7) NOT NULL DEFAULT '#6B7280',
  contact_email VARCHAR(128) NOT NULL,
  token_a TEXT,
  token_b TEXT,
  token_c TEXT,
  token_a_used_at TIMESTAMPTZ,
  ocpi_version VARCHAR(8) DEFAULT '2.2.1',
  handshake_status VARCHAR(16) NOT NULL DEFAULT 'NOT_STARTED',
  handshake_log JSONB,
  bank_name VARCHAR(64),
  bank_account_holder VARCHAR(128),
  bank_account_no VARCHAR(32),
  payout_cycle VARCHAR(16) DEFAULT 'MONTHLY',
  platform_fee_pct NUMERIC(4,2) NOT NULL DEFAULT 5.00,
  status VARCHAR(32) NOT NULL DEFAULT 'PENDING',
  created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
  UNIQUE(country_code, party_id)
);

-- Records the latest handshake check for the portal. Older databases pick up
-- the column when Migrate runs this idempotent file again.
ALTER TABLE cpo_partners ADD COLUMN IF NOT EXISTS handshake_at TIMESTAMPTZ;

-- ===================== Portal users =====================
CREATE TABLE IF NOT EXISTS portal_users (
  id VARCHAR(64) PRIMARY KEY,
  email VARCHAR(128) UNIQUE NOT NULL,
  password_hash TEXT NOT NULL,
  name VARCHAR(128) NOT NULL,
  role VARCHAR(16) NOT NULL,
  cpo_id VARCHAR(64) REFERENCES cpo_partners(id),
  created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);

-- ===================== Drivers =====================
CREATE TABLE IF NOT EXISTS users (
  id VARCHAR(64) PRIMARY KEY,
  name VARCHAR(128) NOT NULL,
  email VARCHAR(128) UNIQUE NOT NULL,
  password_hash TEXT NOT NULL,
  created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS vehicles (
  id VARCHAR(64) PRIMARY KEY,
  user_id VARCHAR(64) NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name VARCHAR(128) NOT NULL,
  license_plate VARCHAR(32) NOT NULL,
  connector_type VARCHAR(16) NOT NULL,
  battery_capacity NUMERIC(6,2) NOT NULL,
  is_default BOOLEAN NOT NULL DEFAULT FALSE,
  created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP   -- NEW: used to promote the oldest vehicle
);
-- Idempotent upgrade for databases created before vehicles.created_at existed.
ALTER TABLE vehicles ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP;
CREATE UNIQUE INDEX IF NOT EXISTS one_default_vehicle_per_user ON vehicles(user_id) WHERE is_default;

CREATE TABLE IF NOT EXISTS wallets (
  id VARCHAR(64) PRIMARY KEY,
  user_id VARCHAR(64) UNIQUE NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  balance NUMERIC(10,2) NOT NULL DEFAULT 0.00 CHECK (balance >= 0),
  currency VARCHAR(3) NOT NULL DEFAULT 'MYR',
  updated_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS wallet_transactions (
  id VARCHAR(64) PRIMARY KEY,
  wallet_id VARCHAR(64) NOT NULL REFERENCES wallets(id) ON DELETE CASCADE,
  type VARCHAR(32) NOT NULL,
  amount NUMERIC(10,2) NOT NULL CHECK (amount > 0),
  payment_method VARCHAR(64) NOT NULL,
  reference_id TEXT NOT NULL DEFAULT '',
  status VARCHAR(32) NOT NULL DEFAULT 'SUCCESS',
  created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS idempotency_keys (
  key VARCHAR(128) NOT NULL,
  user_id VARCHAR(64) NOT NULL,
  endpoint VARCHAR(64) NOT NULL,
  response_status INT NOT NULL,
  response_body JSONB NOT NULL,
  created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (key, user_id, endpoint)
);

-- ===================== Network =====================
CREATE TABLE IF NOT EXISTS stations (
  id VARCHAR(64) PRIMARY KEY,
  cpo_id VARCHAR(64) NOT NULL REFERENCES cpo_partners(id),
  ocpi_location_id VARCHAR(64),
  name VARCHAR(128) NOT NULL,
  address TEXT NOT NULL,
  city VARCHAR(64) NOT NULL,
  latitude NUMERIC(9,6) NOT NULL,
  longitude NUMERIC(9,6) NOT NULL,
  total_bays INT NOT NULL DEFAULT 1,
  available_bays INT NOT NULL DEFAULT 1,
  is_active BOOLEAN NOT NULL DEFAULT TRUE,
  created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS stations_geo ON stations(latitude, longitude);
-- A CPO identifies its locations by (country_code, party_id, location id); the
-- location receiver resolves a push to its station row by ocpi_location_id,
-- so the pair must be unique per partner. The index is partial because a
-- ChargeNet-created station may have no OCPI identity yet.
CREATE UNIQUE INDEX IF NOT EXISTS stations_ocpi_location
  ON stations(cpo_id, ocpi_location_id) WHERE ocpi_location_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS tariffs (
  id VARCHAR(64) PRIMARY KEY,
  cpo_id VARCHAR(64) NOT NULL REFERENCES cpo_partners(id),
  currency VARCHAR(3) NOT NULL DEFAULT 'MYR',
  price_per_kwh NUMERIC(6,2) NOT NULL,
  last_updated TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS connectors (
  id VARCHAR(64) PRIMARY KEY,
  station_id VARCHAR(64) NOT NULL REFERENCES stations(id) ON DELETE CASCADE,
  evse_uid VARCHAR(64) UNIQUE NOT NULL,
  ocpi_connector_id VARCHAR(16),
  standard VARCHAR(16) NOT NULL,
  format VARCHAR(16) NOT NULL,
  power_type VARCHAR(8) NOT NULL,
  max_power_kw NUMERIC(6,2) NOT NULL,
  tariff_id VARCHAR(64) REFERENCES tariffs(id),
  tariff_per_kwh NUMERIC(6,2) NOT NULL,
  status VARCHAR(32) NOT NULL DEFAULT 'AVAILABLE',
  last_updated TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);
-- The CPO's connector id ("1", "2", …) inside its EVSE. ChargeNet needs
-- it to address a START_SESSION command at the right plug, and stores it next to
-- the EVSE uid the driver's QR scan carries.
ALTER TABLE connectors ADD COLUMN IF NOT EXISTS ocpi_connector_id VARCHAR(16);

-- ===================== Sessions & CDRs =====================
CREATE TABLE IF NOT EXISTS charging_sessions (
  id VARCHAR(64) PRIMARY KEY,
  user_id VARCHAR(64) NOT NULL REFERENCES users(id),
  vehicle_id VARCHAR(64) REFERENCES vehicles(id) ON DELETE SET NULL,
  cpo_id VARCHAR(64) NOT NULL REFERENCES cpo_partners(id),
  station_id VARCHAR(64) NOT NULL REFERENCES stations(id),
  connector_id VARCHAR(64) NOT NULL REFERENCES connectors(id),
  evse_uid VARCHAR(64) NOT NULL,
  ocpi_session_id VARCHAR(64),
  status VARCHAR(32) NOT NULL,
  start_time TIMESTAMPTZ NOT NULL,
  end_time TIMESTAMPTZ,
  duration_seconds INT NOT NULL DEFAULT 0,
  kwh_delivered NUMERIC(8,3) NOT NULL DEFAULT 0,
  current_power_kw NUMERIC(6,2) NOT NULL DEFAULT 0,
  battery_soc NUMERIC(5,2) NOT NULL DEFAULT 20,
  target_soc INT NOT NULL DEFAULT 80,
  tariff_per_kwh NUMERIC(6,2) NOT NULL,
  total_cost NUMERIC(10,2) NOT NULL DEFAULT 0,
  balance_after NUMERIC(10,2),
  stop_reason VARCHAR(32),                         -- USER|TARGET_REACHED|BALANCE_EXHAUSTED|FAULT|TIMEOUT|REMOTE_STOPPED
  cdr_id VARCHAR(64)
);
CREATE UNIQUE INDEX IF NOT EXISTS one_active_session_per_user
  ON charging_sessions(user_id) WHERE status IN ('PENDING','CHARGING');
CREATE UNIQUE INDEX IF NOT EXISTS one_active_session_per_connector
  ON charging_sessions(connector_id) WHERE status IN ('PENDING','CHARGING');

CREATE TABLE IF NOT EXISTS cdrs (
  id VARCHAR(64) PRIMARY KEY,
  session_id VARCHAR(64) UNIQUE NOT NULL REFERENCES charging_sessions(id),
  cpo_id VARCHAR(64) NOT NULL REFERENCES cpo_partners(id),
  user_id VARCHAR(64) NOT NULL REFERENCES users(id),
  total_energy_kwh NUMERIC(8,3) NOT NULL,
  total_time_seconds INT NOT NULL,
  gross_amount NUMERIC(10,2) NOT NULL,
  platform_fee NUMERIC(10,2) NOT NULL,
  net_payout NUMERIC(10,2) NOT NULL,
  currency VARCHAR(3) NOT NULL DEFAULT 'MYR',
  cdr_json JSONB NOT NULL,
  created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);


-- ============================================================================
-- Non-user seed data (idempotent).
-- ============================================================================

-- ---- CPO partners (ACTIVE, handshake VERIFIED, dev tokens) ----
INSERT INTO cpo_partners
  (id, name, slug, country_code, party_id, base_url, color_hex, contact_email,
   token_a, token_b, token_c, handshake_status, status, platform_fee_pct,
   bank_name, bank_account_holder, bank_account_no, payout_cycle)
VALUES
  ('cpo_voltgo', 'VoltGo', 'voltgo', 'MY', 'VLT',
   'http://localhost:8080/emulator/voltgo/ocpi', '#0D9488', 'ops@voltgo.my',
   NULL, 'voltgo-token-b-dev', 'voltgo-token-c-dev', 'VERIFIED', 'ACTIVE', 5.00,
   'Maybank', 'VoltGo Sdn Bhd', '514012345678', 'MONTHLY'),
  ('cpo_chargesync', 'ChargeSync', 'chargesync', 'MY', 'CSY',
   'http://localhost:8080/emulator/chargesync/ocpi', '#2563EB', 'ops@chargesync.my',
   NULL, 'chargesync-token-b-dev', 'chargesync-token-c-dev', 'VERIFIED', 'ACTIVE', 5.00,
   'CIMB Bank', 'ChargeSync Sdn Bhd', '7001234567', 'MONTHLY'),
  ('cpo_ecowatt', 'EcoWatt', 'ecowatt', 'MY', 'ECO',
   'http://localhost:8080/emulator/ecowatt/ocpi', '#16A34A', 'ops@ecowatt.my',
   NULL, 'ecowatt-token-b-dev', 'ecowatt-token-c-dev', 'VERIFIED', 'ACTIVE', 5.00,
   'Public Bank', 'EcoWatt Sdn Bhd', '3123456789', 'MONTHLY')
ON CONFLICT (id) DO NOTHING;

-- ---- Tariffs (simple per-kWh OCPI model) ----
-- One tariff per price tier. A CPO prices by bay power (plan 2.10: VoltGo "RM
-- 1.60-1.80/kWh" by kW band), and OCPI carries a connector's price through the
-- tariff id it references — so every distinct price needs its own id, or a
-- location/tariff round trip would collapse the tiers onto a single price.
INSERT INTO tariffs (id, cpo_id, currency, price_per_kwh) VALUES
  ('trf_voltgo_350',     'cpo_voltgo',     'MYR', 1.80),
  ('trf_voltgo_250',     'cpo_voltgo',     'MYR', 1.70),
  ('trf_voltgo_180',     'cpo_voltgo',     'MYR', 1.60),
  ('trf_chargesync_120', 'cpo_chargesync', 'MYR', 1.50),
  ('trf_chargesync_60',  'cpo_chargesync', 'MYR', 1.20),
  ('trf_ecowatt_ac',     'cpo_ecowatt',    'MYR', 0.90),
  ('trf_ecowatt_dc',     'cpo_ecowatt',    'MYR', 1.10)
ON CONFLICT (id) DO NOTHING;

-- ---- Stations ----
INSERT INTO stations
  (id, cpo_id, ocpi_location_id, name, address, city, latitude, longitude,
   total_bays, available_bays, is_active)
VALUES
  -- VoltGo (highway DC)
  ('stn_vlt_sgb', 'cpo_voltgo', 'VLT-SGB', 'VoltGo Sungai Buloh R&R',
   'Lebuhraya Utara-Selatan, Sungai Buloh', 'Sungai Buloh', 3.210500, 101.579000, 3, 2, TRUE),
  ('stn_vlt_gsr', 'cpo_voltgo', 'VLT-GSR', 'VoltGo Genting Sempah R&R',
   'Lebuhraya Kuala Lumpur-Karak, Genting Sempah', 'Genting Sempah', 3.365000, 101.787000, 2, 2, TRUE),
  ('stn_vlt_srr', 'cpo_voltgo', 'VLT-SRR', 'VoltGo Seremban R&R',
   'Lebuhraya Utara-Selatan, Seremban', 'Seremban', 2.734000, 101.918000, 4, 3, TRUE),
  ('stn_vlt_pjs', 'cpo_voltgo', 'VLT-PJS', 'VoltGo Putrajaya Sentral',
   'Putrajaya Sentral, Presint 7', 'Putrajaya', 2.926400, 101.676400, 3, 3, TRUE),
  -- ChargeSync (mall hubs DC)
  ('stn_csy_mvl', 'cpo_chargesync', 'CSY-MVL', 'ChargeSync Mid Valley',
   'Mid Valley Megamall, Lingkaran Syed Putra', 'Kuala Lumpur', 3.117700, 101.677000, 4, 3, TRUE),
  ('stn_csy_pav', 'cpo_chargesync', 'CSY-PAV', 'ChargeSync Pavilion KL',
   'Pavilion Kuala Lumpur, Bukit Bintang', 'Kuala Lumpur', 3.148800, 101.713400, 3, 2, TRUE),
  ('stn_csy_1u',  'cpo_chargesync', 'CSY-1U',  'ChargeSync 1 Utama',
   '1 Utama Shopping Centre, Bandar Utama', 'Petaling Jaya', 3.150300, 101.615000, 4, 4, TRUE),
  ('stn_csy_swp', 'cpo_chargesync', 'CSY-SWP', 'ChargeSync Sunway Pyramid',
   'Sunway Pyramid, Bandar Sunway', 'Subang Jaya', 3.073100, 101.606700, 3, 2, TRUE),
  ('stn_csy_ioi', 'cpo_chargesync', 'CSY-IOI', 'ChargeSync IOI City Mall',
   'IOI City Mall, Putrajaya', 'Putrajaya', 2.970700, 101.716500, 3, 3, TRUE),
  -- EcoWatt (urban AC + DC)
  ('stn_eco_klc', 'cpo_ecowatt', 'ECO-KLC', 'EcoWatt KLCC Hotel',
   'Jalan Kia Peng, Kuala Lumpur', 'Kuala Lumpur', 3.157800, 101.712300, 4, 4, TRUE),
  ('stn_eco_bsg', 'cpo_ecowatt', 'ECO-BSG', 'EcoWatt Bangsar South',
   'The Vertical, Bangsar South', 'Kuala Lumpur', 3.110000, 101.665000, 4, 3, TRUE),
  ('stn_eco_ttd', 'cpo_ecowatt', 'ECO-TTD', 'EcoWatt TTDI Plaza',
   'TTDI Plaza, Taman Tun Dr Ismail', 'Kuala Lumpur', 3.137000, 101.631000, 3, 2, TRUE),
  ('stn_eco_cyb', 'cpo_ecowatt', 'ECO-CYB', 'EcoWatt Cyberjaya',
   'Persiaran Multimedia, Cyberjaya', 'Cyberjaya', 2.918900, 101.650000, 4, 4, TRUE),
  ('stn_eco_bbg', 'cpo_ecowatt', 'ECO-BBG', 'EcoWatt Bukit Bintang Hotel',
   'Jalan Bukit Bintang, Kuala Lumpur', 'Kuala Lumpur', 3.146600, 101.711000, 2, 2, TRUE)
ON CONFLICT (id) DO NOTHING;

-- ---- Connectors / EVSEs (EVSE uid = {PARTY}-{STATION3}-{NN}) ----
INSERT INTO connectors
  (id, station_id, evse_uid, standard, format, power_type, max_power_kw,
   tariff_id, tariff_per_kwh, status)
VALUES
  -- VoltGo: CCS2 DC highway fast chargers
  ('con_vlt_sgb_01', 'stn_vlt_sgb', 'VLT-SGB-01', 'CCS2', 'CABLE', 'DC', 350.00, 'trf_voltgo_350', 1.80, 'AVAILABLE'),
  ('con_vlt_sgb_02', 'stn_vlt_sgb', 'VLT-SGB-02', 'CCS2', 'CABLE', 'DC', 180.00, 'trf_voltgo_180', 1.60, 'CHARGING'),
  ('con_vlt_sgb_03', 'stn_vlt_sgb', 'VLT-SGB-03', 'CCS2', 'CABLE', 'DC', 180.00, 'trf_voltgo_180', 1.60, 'AVAILABLE'),
  ('con_vlt_gsr_01', 'stn_vlt_gsr', 'VLT-GSR-01', 'CCS2', 'CABLE', 'DC', 180.00, 'trf_voltgo_180', 1.60, 'AVAILABLE'),
  ('con_vlt_gsr_02', 'stn_vlt_gsr', 'VLT-GSR-02', 'CCS2', 'CABLE', 'DC', 180.00, 'trf_voltgo_180', 1.60, 'AVAILABLE'),
  ('con_vlt_srr_01', 'stn_vlt_srr', 'VLT-SRR-01', 'CCS2', 'CABLE', 'DC', 350.00, 'trf_voltgo_350', 1.80, 'AVAILABLE'),
  ('con_vlt_srr_02', 'stn_vlt_srr', 'VLT-SRR-02', 'CCS2', 'CABLE', 'DC', 350.00, 'trf_voltgo_350', 1.80, 'AVAILABLE'),
  ('con_vlt_srr_03', 'stn_vlt_srr', 'VLT-SRR-03', 'CCS2', 'CABLE', 'DC', 180.00, 'trf_voltgo_180', 1.60, 'AVAILABLE'),
  ('con_vlt_srr_04', 'stn_vlt_srr', 'VLT-SRR-04', 'CCS2', 'CABLE', 'DC', 180.00, 'trf_voltgo_180', 1.60, 'INOPERATIVE'),
  ('con_vlt_pjs_01', 'stn_vlt_pjs', 'VLT-PJS-01', 'CCS2', 'CABLE', 'DC', 250.00, 'trf_voltgo_250', 1.70, 'AVAILABLE'),
  ('con_vlt_pjs_02', 'stn_vlt_pjs', 'VLT-PJS-02', 'CCS2', 'CABLE', 'DC', 180.00, 'trf_voltgo_180', 1.60, 'AVAILABLE'),
  ('con_vlt_pjs_03', 'stn_vlt_pjs', 'VLT-PJS-03', 'CCS2', 'CABLE', 'DC', 180.00, 'trf_voltgo_180', 1.60, 'AVAILABLE'),
  -- ChargeSync: CCS2 DC mall hubs
  ('con_csy_mvl_01', 'stn_csy_mvl', 'CSY-MVL-01', 'CCS2', 'CABLE', 'DC', 120.00, 'trf_chargesync_120', 1.50, 'AVAILABLE'),
  ('con_csy_mvl_02', 'stn_csy_mvl', 'CSY-MVL-02', 'CCS2', 'CABLE', 'DC', 120.00, 'trf_chargesync_120', 1.50, 'CHARGING'),
  ('con_csy_mvl_03', 'stn_csy_mvl', 'CSY-MVL-03', 'CCS2', 'CABLE', 'DC', 60.00, 'trf_chargesync_60', 1.20, 'AVAILABLE'),
  ('con_csy_mvl_04', 'stn_csy_mvl', 'CSY-MVL-04', 'CCS2', 'CABLE', 'DC', 60.00, 'trf_chargesync_60', 1.20, 'AVAILABLE'),
  ('con_csy_pav_01', 'stn_csy_pav', 'CSY-PAV-01', 'CCS2', 'CABLE', 'DC', 120.00, 'trf_chargesync_120', 1.50, 'AVAILABLE'),
  ('con_csy_pav_02', 'stn_csy_pav', 'CSY-PAV-02', 'CCS2', 'CABLE', 'DC', 60.00, 'trf_chargesync_60', 1.20, 'AVAILABLE'),
  ('con_csy_pav_03', 'stn_csy_pav', 'CSY-PAV-03', 'CCS2', 'CABLE', 'DC', 60.00, 'trf_chargesync_60', 1.20, 'BLOCKED'),
  ('con_csy_1u_01', 'stn_csy_1u', 'CSY-1U-01', 'CCS2', 'CABLE', 'DC', 120.00, 'trf_chargesync_120', 1.50, 'AVAILABLE'),
  ('con_csy_1u_02', 'stn_csy_1u', 'CSY-1U-02', 'CCS2', 'CABLE', 'DC', 120.00, 'trf_chargesync_120', 1.50, 'AVAILABLE'),
  ('con_csy_1u_03', 'stn_csy_1u', 'CSY-1U-03', 'CCS2', 'CABLE', 'DC', 60.00, 'trf_chargesync_60', 1.20, 'AVAILABLE'),
  ('con_csy_1u_04', 'stn_csy_1u', 'CSY-1U-04', 'CCS2', 'CABLE', 'DC', 60.00, 'trf_chargesync_60', 1.20, 'AVAILABLE'),
  ('con_csy_swp_01', 'stn_csy_swp', 'CSY-SWP-01', 'CCS2', 'CABLE', 'DC', 60.00, 'trf_chargesync_60', 1.20, 'AVAILABLE'),
  ('con_csy_swp_02', 'stn_csy_swp', 'CSY-SWP-02', 'CCS2', 'CABLE', 'DC', 60.00, 'trf_chargesync_60', 1.20, 'AVAILABLE'),
  ('con_csy_swp_03', 'stn_csy_swp', 'CSY-SWP-03', 'CCS2', 'CABLE', 'DC', 60.00, 'trf_chargesync_60', 1.20, 'CHARGING'),
  ('con_csy_ioi_01', 'stn_csy_ioi', 'CSY-IOI-01', 'CCS2', 'CABLE', 'DC', 120.00, 'trf_chargesync_120', 1.50, 'AVAILABLE'),
  ('con_csy_ioi_02', 'stn_csy_ioi', 'CSY-IOI-02', 'CCS2', 'CABLE', 'DC', 60.00, 'trf_chargesync_60', 1.20, 'AVAILABLE'),
  ('con_csy_ioi_03', 'stn_csy_ioi', 'CSY-IOI-03', 'CCS2', 'CABLE', 'DC', 60.00, 'trf_chargesync_60', 1.20, 'AVAILABLE'),
  -- EcoWatt: Type 2 AC + CCS2 DC
  ('con_eco_klc_01', 'stn_eco_klc', 'ECO-KLC-01', 'TYPE2', 'SOCKET', 'AC', 22.00, 'trf_ecowatt_ac', 0.90, 'AVAILABLE'),
  ('con_eco_klc_02', 'stn_eco_klc', 'ECO-KLC-02', 'TYPE2', 'SOCKET', 'AC', 22.00, 'trf_ecowatt_ac', 0.90, 'AVAILABLE'),
  ('con_eco_klc_03', 'stn_eco_klc', 'ECO-KLC-03', 'TYPE2', 'SOCKET', 'AC', 22.00, 'trf_ecowatt_ac', 0.90, 'AVAILABLE'),
  ('con_eco_klc_04', 'stn_eco_klc', 'ECO-KLC-04', 'CCS2', 'CABLE', 'DC', 50.00, 'trf_ecowatt_dc', 1.10, 'AVAILABLE'),
  ('con_eco_bsg_01', 'stn_eco_bsg', 'ECO-BSG-01', 'TYPE2', 'SOCKET', 'AC', 22.00, 'trf_ecowatt_ac', 0.90, 'AVAILABLE'),
  ('con_eco_bsg_02', 'stn_eco_bsg', 'ECO-BSG-02', 'TYPE2', 'SOCKET', 'AC', 22.00, 'trf_ecowatt_ac', 0.90, 'AVAILABLE'),
  ('con_eco_bsg_03', 'stn_eco_bsg', 'ECO-BSG-03', 'TYPE2', 'SOCKET', 'AC', 22.00, 'trf_ecowatt_ac', 0.90, 'CHARGING'),
  ('con_eco_bsg_04', 'stn_eco_bsg', 'ECO-BSG-04', 'TYPE2', 'SOCKET', 'AC', 22.00, 'trf_ecowatt_ac', 0.90, 'AVAILABLE'),
  ('con_eco_ttd_01', 'stn_eco_ttd', 'ECO-TTD-01', 'TYPE2', 'SOCKET', 'AC', 22.00, 'trf_ecowatt_ac', 0.90, 'AVAILABLE'),
  ('con_eco_ttd_02', 'stn_eco_ttd', 'ECO-TTD-02', 'TYPE2', 'SOCKET', 'AC', 22.00, 'trf_ecowatt_ac', 0.90, 'AVAILABLE'),
  ('con_eco_ttd_03', 'stn_eco_ttd', 'ECO-TTD-03', 'CCS2', 'CABLE', 'DC', 50.00, 'trf_ecowatt_dc', 1.10, 'INOPERATIVE'),
  ('con_eco_cyb_01', 'stn_eco_cyb', 'ECO-CYB-01', 'TYPE2', 'SOCKET', 'AC', 22.00, 'trf_ecowatt_ac', 0.90, 'AVAILABLE'),
  ('con_eco_cyb_02', 'stn_eco_cyb', 'ECO-CYB-02', 'TYPE2', 'SOCKET', 'AC', 22.00, 'trf_ecowatt_ac', 0.90, 'AVAILABLE'),
  ('con_eco_cyb_03', 'stn_eco_cyb', 'ECO-CYB-03', 'TYPE2', 'SOCKET', 'AC', 22.00, 'trf_ecowatt_ac', 0.90, 'AVAILABLE'),
  ('con_eco_cyb_04', 'stn_eco_cyb', 'ECO-CYB-04', 'TYPE2', 'SOCKET', 'AC', 22.00, 'trf_ecowatt_ac', 0.90, 'AVAILABLE'),
  ('con_eco_bbg_01', 'stn_eco_bbg', 'ECO-BBG-01', 'TYPE2', 'SOCKET', 'AC', 22.00, 'trf_ecowatt_ac', 0.90, 'AVAILABLE'),
  ('con_eco_bbg_02', 'stn_eco_bbg', 'ECO-BBG-02', 'TYPE2', 'SOCKET', 'AC', 22.00, 'trf_ecowatt_ac', 0.90, 'AVAILABLE')
ON CONFLICT (id) DO NOTHING;

