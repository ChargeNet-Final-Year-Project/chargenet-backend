// Package store — the OCPI synchronisation half of the API contract.
//
// A CPO pushes what it owns: its locations and their EVSEs, its tariffs, the
// live and final state of the sessions ChargeNet started on it, and a CDR when
// a charge is over. Everything those pushes change in the database lives here,
// so the receiver in internal/ocpi is a pure translator: it maps OCPI names
// onto the canonical ones and calls one method per push.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// OCPILocationInput is one pushed location: the station it describes plus every
// connector the CPO reports under it.
type OCPILocationInput struct {
	Region      string
	CountryCode string
	CpoID       string
	LocationID  string
	Name        string
	Address     string
	City        string
	Latitude    float64
	Longitude   float64
	Connectors  []OCPIConnectorInput
}

// OCPIConnectorInput is one pushed connector. TariffID is the CPO's tariff
// identifier (the OCPI connector's `tariff_ids`); the price itself comes from
// the tariff push, so a location that arrives before its tariff still lands —
// priced at zero until the tariff receiver corrects it.
type OCPIConnectorInput struct {
	EVSEUID         string
	OCPIConnectorID string
	Standard        string
	Format          string
	PowerType       string
	MaxPowerKW      float64
	TariffID        string
	Status          string
}

// OCPITariffInput holds a pushed tariff. The billing model keeps the first
// ENERGY price component.
type OCPITariffInput struct {
	Unsupported bool
	RawJSON     []byte
	CpoID       string
	ID          string
	Currency    string
	PricePerKWh float64
}

// UpsertOCPILocation stores a pushed location and its connectors, returning the
// ChargeNet station id.
//
// The row is matched on `(cpo_id, ocpi_location_id)`, so re-pushing the same
// location updates the station the driver app already shows instead of creating
// a second one — which is what makes a CPO's periodic full push idempotent. The
// bay counts are recomputed from the connectors that end up stored, because a
// CPO does not send them: OCPI counts EVSEs and connectors, the mobile counts
// bays.
//
// A connector the CPO no longer reports is removed — a Location PUT replaces
// the location — unless a session still references it, which the foreign key
// forbids and history must keep.
//
// The tariff ids the connectors name are recorded as they arrive: one ChargeNet
// has no price for yet gets a zero-priced tariff row. A later tariff push sets
// the price without a foreign-key failure during location ingestion.
func (s *Store) UpsertOCPILocation(ctx context.Context, in OCPILocationInput) (string, error) {
	if strings.TrimSpace(in.LocationID) == "" {
		return "", fmt.Errorf("store: location push without an id")
	}
	if len(in.Connectors) == 0 {
		return "", fmt.Errorf("store: location %s has no connectors", in.LocationID)
	}
	if in.CountryCode == "" {
		in.CountryCode = "MY"
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = in.LocationID
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var stationID string
	err = tx.QueryRowContext(ctx,
		`INSERT INTO stations (id, cpo_id, ocpi_location_id, name, address, city,
		                       latitude, longitude, total_bays, available_bays, is_active,country_code,region)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 0, 0, TRUE,$9,$10)
		 ON CONFLICT (cpo_id, ocpi_location_id) WHERE ocpi_location_id IS NOT NULL
		 DO UPDATE SET name = EXCLUDED.name, address = EXCLUDED.address, city = EXCLUDED.city,
		               latitude = EXCLUDED.latitude, longitude = EXCLUDED.longitude,
		               is_active = TRUE,country_code=EXCLUDED.country_code,region=CASE WHEN EXCLUDED.region='' THEN stations.region ELSE EXCLUDED.region END
		 RETURNING id`,
		NewID("stn"), in.CpoID, in.LocationID, name, in.Address, in.City,
		in.Latitude, in.Longitude, in.CountryCode, in.Region).Scan(&stationID)
	if err != nil {
		return "", fmt.Errorf("store: upsert location %s: %w", in.LocationID, err)
	}

	prices, err := tariffPricesTx(ctx, tx, in.CpoID)
	if err != nil {
		return "", err
	}

	// A location may arrive before the tariff it references — OCPI puts no order
	// on the modules — so an id ChargeNet has no price for yet gets a zero-priced
	// row: the connector's foreign key holds, the driver sees the bay at nothing,
	// and the tariff push that follows fills the price in and reprices it.
	for _, id := range missingTariffs(in.Connectors, prices) {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO tariffs (id, cpo_id, currency, price_per_kwh, last_updated)
			 VALUES ($1, $2, $3, 0, NOW()) ON CONFLICT (id) DO NOTHING`,
			id, in.CpoID, CurrencyMYR); err != nil {
			return "", fmt.Errorf("store: placeholder tariff %s: %w", id, err)
		}
		prices[id] = 0
	}

	kept := make([]string, 0, len(in.Connectors))
	for _, c := range in.Connectors {
		if strings.TrimSpace(c.EVSEUID) == "" {
			return "", fmt.Errorf("store: location %s has a connector without an EVSE uid", in.LocationID)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO connectors (id, station_id, evse_uid, ocpi_connector_id, standard, format,
			                         power_type, max_power_kw, tariff_id, tariff_per_kwh, status)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			 ON CONFLICT (evse_uid) DO UPDATE
			 SET station_id = EXCLUDED.station_id, ocpi_connector_id = EXCLUDED.ocpi_connector_id,
			     standard = EXCLUDED.standard, format = EXCLUDED.format,
			     power_type = EXCLUDED.power_type, max_power_kw = EXCLUDED.max_power_kw,
			     tariff_id = EXCLUDED.tariff_id, tariff_per_kwh = EXCLUDED.tariff_per_kwh,
			     status = CASE WHEN EXISTS(SELECT 1 FROM charging_sessions cs WHERE cs.connector_id=connectors.id AND cs.status IN ('PENDING','CHARGING')) THEN connectors.status ELSE EXCLUDED.status END,
 last_updated=NOW() WHERE connectors.station_id=EXCLUDED.station_id`,
			NewID("con"), stationID, c.EVSEUID, nullString(c.OCPIConnectorID), c.Standard, c.Format,
			c.PowerType, c.MaxPowerKW, nullString(c.TariffID), prices[c.TariffID], c.Status); err != nil {
			return "", fmt.Errorf("store: upsert connector %s: %w", c.EVSEUID, err)
		}
		kept = append(kept, c.EVSEUID)
	}

	// Drop the EVSEs the CPO no longer lists, but never one a session points at.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM connectors
		  WHERE station_id = $1 AND NOT (evse_uid = ANY($2))
		    AND NOT EXISTS (SELECT 1 FROM charging_sessions cs WHERE cs.connector_id = connectors.id)`,
		stationID, kept); err != nil {
		return "", fmt.Errorf("store: prune connectors of %s: %w", in.LocationID, err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE stations
		    SET total_bays = (SELECT count(*) FROM connectors WHERE station_id = $1),
		        available_bays = (SELECT count(*) FROM connectors WHERE station_id = $1 AND status = 'AVAILABLE')
		  WHERE id = $1`, stationID); err != nil {
		return "", fmt.Errorf("store: recount bays of %s: %w", in.LocationID, err)
	}

	if err := tx.Commit(); err != nil {
		return "", err
	}
	return stationID, nil
}

// UpsertOCPITariff stores a pushed tariff and reprices every connector that
// lists it, which is how a CPO's price change reaches the driver app without a
// connector push.
func (s *Store) UpsertOCPITariff(ctx context.Context, in OCPITariffInput) error {
	var owner string
	err := s.DB.QueryRowContext(ctx, `SELECT cpo_id FROM tariffs WHERE id=$1`, in.ID).Scan(&owner)
	if err == nil && owner != in.CpoID {
		return fmt.Errorf("tariff identifier belongs to a different CPO")
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	currency := strings.TrimSpace(in.Currency)
	if currency == "" {
		currency = CurrencyMYR
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO tariffs (id, cpo_id, currency, price_per_kwh, last_updated,pricing_supported,ocpi_json)
		 VALUES ($1, $2, $3, $4, NOW(),$5,$6::jsonb)
		 ON CONFLICT (id) DO UPDATE
		 SET currency = EXCLUDED.currency, price_per_kwh = EXCLUDED.price_per_kwh,
		     pricing_supported=EXCLUDED.pricing_supported,ocpi_json=EXCLUDED.ocpi_json,
		     last_updated = CASE WHEN tariffs.currency IS DISTINCT FROM EXCLUDED.currency OR tariffs.price_per_kwh IS DISTINCT FROM EXCLUDED.price_per_kwh OR tariffs.pricing_supported IS DISTINCT FROM EXCLUDED.pricing_supported THEN NOW() ELSE tariffs.last_updated END
 WHERE tariffs.cpo_id=EXCLUDED.cpo_id`,
		in.ID, in.CpoID, currency, in.PricePerKWh, !in.Unsupported, nullableJSON(in.RawJSON)); err != nil {
		return fmt.Errorf("store: upsert tariff %s: %w", in.ID, err)
	}

	// Only this partner's connectors: a tariff id is the CPO's own string, so
	// another partner may legitimately use the same one.
	if _, err := tx.ExecContext(ctx,
		`UPDATE connectors SET tariff_per_kwh = $2, last_updated = NOW()
		  WHERE tariff_id = $1
		    AND station_id IN (SELECT id FROM stations WHERE cpo_id = $3)`,
		in.ID, in.PricePerKWh, in.CpoID); err != nil {
		return fmt.Errorf("store: reprice connectors of tariff %s: %w", in.ID, err)
	}

	return tx.Commit()
}

// SetOCPIConnectorStatus changes the status of one of a CPO's connectors,
// addressed by its EVSE uid, and recomputes the station's available bays. It is
// the PATCH half of a Location push.
func (s *Store) SetOCPIConnectorStatus(ctx context.Context, cpoID, evseUID, status string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	// An EVSE that carries more than one socket became more than one ChargeNet
	// row, suffixed with the OCPI connector id (see ocpi.EVSEUIDFor), so every
	// row of the EVSE moves together: the CPO reported the state of the charge
	// point, not of one of its plugs.
	rows, err := tx.QueryContext(ctx,
		`SELECT k.id
		   FROM connectors k JOIN stations s ON s.id = k.station_id
		  WHERE split_part(k.evse_uid, ':', 1) = $1 AND s.cpo_id = $2
		  ORDER BY k.evse_uid`, evseUID, cpoID)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return ErrNotFound
	}

	for _, id := range ids {
		if err := setConnectorStatusTx(ctx, tx, id, status); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// OCPICommandTarget contains the fields the OCPI gateway needs to
// address a CPO with a command: how to name the session, where the EVSE is, and
// which connector the CPO knows it by.
type OCPICommandTarget struct {
	SessionID string
	UserID    string
	CPOID     string
	// OCPISessionID is the CPO's own session id, known once it has pushed a
	// Session for ours. It is empty before that.
	OCPISessionID string
	// LocationID is the CPO's location id of the station
	// (stations.ocpi_location_id).
	LocationID string
	EVSEUID    string
	// OCPIConnectorID is the CPO's connector id inside its EVSE.
	OCPIConnectorID string
}

// OCPICommandTarget loads the addressing facts of a ChargeNet session.
func (s *Store) OCPICommandTarget(ctx context.Context, sessionID string) (*OCPICommandTarget, error) {
	t := &OCPICommandTarget{SessionID: sessionID}
	err := s.DB.QueryRowContext(ctx,
		`SELECT cs.user_id, cs.cpo_id, coalesce(cs.ocpi_session_id, ''), cs.evse_uid,
		        coalesce(s.ocpi_location_id, ''), coalesce(k.ocpi_connector_id, '')
		   FROM charging_sessions cs
		   JOIN stations s ON s.id = cs.station_id
		   JOIN connectors k ON k.id = cs.connector_id
		  WHERE cs.id = $1`, sessionID).
		Scan(&t.UserID, &t.CPOID, &t.OCPISessionID, &t.EVSEUID, &t.LocationID, &t.OCPIConnectorID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// OCPIUnlockTarget loads the same addressing facts for an EVSE, for
// UNLOCK_CONNECTOR — the one command that has no session.
func (s *Store) OCPIUnlockTarget(ctx context.Context, evseUID string) (*OCPICommandTarget, error) {
	t := &OCPICommandTarget{EVSEUID: evseUID}
	err := s.DB.QueryRowContext(ctx,
		`SELECT s.cpo_id, coalesce(s.ocpi_location_id, ''), k.evse_uid, coalesce(k.ocpi_connector_id, '')
		   FROM connectors k JOIN stations s ON s.id = k.station_id
		  WHERE k.evse_uid = $1`, evseUID).
		Scan(&t.CPOID, &t.LocationID, &t.EVSEUID, &t.OCPIConnectorID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// SessionByOCPI resolves the ChargeNet session a CPO push is about.
//
// Two links are tried, in order: the CPO's own session id, which ChargeNet
// stores the first time the CPO names the session, and the
// `authorization_reference` — ChargeNet's session id — which the START_SESSION
// command carries and which is therefore the only handle a CPO has on its first
// push. Both are scoped to the partner that authenticated the call.
func (s *Store) SessionByOCPI(ctx context.Context, cpoID, ocpiSessionID, authorizationRef string) (*Session, error) {
	const query = `SELECT ` + sessionColumns + sessionJoins

	if id := strings.TrimSpace(ocpiSessionID); id != "" {
		sess, err := scanSession(s.DB.QueryRowContext(ctx,
			query+` WHERE s.cpo_id = $1 AND s.ocpi_session_id = $2`, cpoID, id))
		if err == nil || !errors.Is(err, ErrNotFound) {
			return sess, err
		}
	}
	if ref := strings.TrimSpace(authorizationRef); ref != "" {
		sess, err := scanSession(s.DB.QueryRowContext(ctx,
			query+` WHERE s.cpo_id = $1 AND s.id = $2`, cpoID, ref))
		if err == nil || !errors.Is(err, ErrNotFound) {
			return sess, err
		}
	}
	return nil, ErrNotFound
}

// UpdateCDRDocument replaces the OCPI document stored for a settled session's
// CDR.
//
// It exists because of the order two messages may arrive in: a CPO that pushes
// the final Session before its CDR settles the session with the document
// ChargeNet built, and the CDR that follows is the CPO's own,
// authoritative record of the charge. Writing it over the built one keeps
// `cdrs.cdr_json` the document the CPO issued, while the money
// columns — the ledger — stay exactly as the settlement committed them.
func (s *Store) UpdateCDRDocument(ctx context.Context, sessionID string, doc []byte) error {
	res, err := s.DB.ExecContext(ctx,
		`UPDATE cdrs SET source_cdr_json = coalesce(source_cdr_json,$2::jsonb) WHERE session_id = $1`, sessionID, string(doc))
	if err != nil {
		return fmt.Errorf("store: store cpo cdr of %s: %w", sessionID, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// CDRDocumentFor returns the document ChargeNet holds for a session's CDR: the
// one the settlement built, which carries the platform's own revenue block and
// is therefore the ledger's account of the charge.
func (s *Store) CDRDocumentFor(ctx context.Context, sessionID string) ([]byte, error) {
	var raw []byte
	err := s.DB.QueryRowContext(ctx, `SELECT cdr_json FROM cdrs WHERE session_id = $1`, sessionID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// OCPICDRDocumentFor returns the document a partner knows as ocpiCDRID — the id
// it posted the CDR under, which is the id its own URL names and the one the
// `Location` header of the push points at.
func (s *Store) OCPICDRDocumentFor(ctx context.Context, cpoID, ocpiCDRID string) ([]byte, error) {
	var raw []byte
	err := s.DB.QueryRowContext(ctx,
		`SELECT coalesce(source_cdr_json,cdr_json) FROM cdrs WHERE cpo_id = $1 AND coalesce(source_cdr_json,cdr_json)->>'id' = $2`, cpoID, ocpiCDRID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// LinkOCPISession records the CPO's own id for one of our sessions the first
// time the CPO names it, which is what lets every later push — and the CDR —
// find the row again. An id already known is never replaced: the first CPO to
// name the session owns that name.
func (s *Store) LinkOCPISession(ctx context.Context, cpoID, sessionID, ocpiSessionID string) error {
	if strings.TrimSpace(ocpiSessionID) == "" {
		return nil
	}
	_, err := s.DB.ExecContext(ctx,
		`UPDATE charging_sessions SET ocpi_session_id = $3
		  WHERE id = $1 AND cpo_id = $2 AND ocpi_session_id IS NULL`, sessionID, cpoID, ocpiSessionID)
	return err
}

// OCPILocationRead is a station read back the way its CPO pushed it: what the emulators's
// emulator renders its Location documents from, and what the OCPI receivers tests assert a
// push stored.
type OCPILocationRead struct {
	Region      string
	CountryCode string
	IsSimulated bool
	ID          string
	CPOID       string
	LocationID  string
	Name        string
	Address     string
	City        string
	Latitude    float64
	Longitude   float64
	TotalBays   int
	Available   int
	IsActive    bool
	Connectors  []OCPIConnectorRead
}

// OCPIConnectorRead is one connector as a CPO document describes it.
type OCPIConnectorRead struct {
	Currency        string
	ID              string
	EVSEUID         string
	OCPIConnectorID string
	Standard        string
	Format          string
	PowerType       string
	MaxPowerKW      float64
	TariffID        string
	TariffPerKWh    float64
	Status          string
}

// GetOCPILocation loads one station by its OCPI identity, connectors included.
func (s *Store) GetOCPILocation(ctx context.Context, cpoID, ocpiLocationID string) (*OCPILocationRead, error) {
	loc := &OCPILocationRead{CPOID: cpoID, LocationID: ocpiLocationID}
	err := s.DB.QueryRowContext(ctx,
		`SELECT id, name, address, city, latitude, longitude, total_bays, available_bays, is_active,country_code,is_simulated,region
		   FROM stations WHERE cpo_id = $1 AND ocpi_location_id = $2`, cpoID, ocpiLocationID).
		Scan(&loc.ID, &loc.Name, &loc.Address, &loc.City, &loc.Latitude, &loc.Longitude,
			&loc.TotalBays, &loc.Available, &loc.IsActive, &loc.CountryCode, &loc.IsSimulated, &loc.Region)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, evse_uid, coalesce(ocpi_connector_id, ''), standard, format, power_type,
		        max_power_kw, coalesce(tariff_id, ''), tariff_per_kwh, status,coalesce((SELECT currency FROM tariffs WHERE id=tariff_id),'MYR')
		   FROM connectors WHERE station_id = $1 ORDER BY evse_uid`, loc.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	loc.Connectors = []OCPIConnectorRead{}
	for rows.Next() {
		var c OCPIConnectorRead
		if err := rows.Scan(&c.ID, &c.EVSEUID, &c.OCPIConnectorID, &c.Standard, &c.Format,
			&c.PowerType, &c.MaxPowerKW, &c.TariffID, &c.TariffPerKWh, &c.Status, &c.Currency); err != nil {
			return nil, err
		}
		loc.Connectors = append(loc.Connectors, c)
	}
	return loc, rows.Err()
}

// ListOCPILocations returns every active location of one partner, connectors
// included: the snapshot the emulators's emulator pushes, and the read side of the
// location receiver.
func (s *Store) ListOCPILocations(ctx context.Context, cpoID string) ([]OCPILocationRead, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT ocpi_location_id FROM stations
		  WHERE cpo_id = $1 AND ocpi_location_id IS NOT NULL AND is_active
		  ORDER BY ocpi_location_id`, cpoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]OCPILocationRead, 0, len(ids))
	for _, id := range ids {
		loc, err := s.GetOCPILocation(ctx, cpoID, id)
		if err != nil {
			return nil, err
		}
		out = append(out, *loc)
	}
	return out, nil
}

// missingTariffs lists the tariff ids a location references that ChargeNet has
// no price for yet, each one once, in the order the connectors name them.
func missingTariffs(connectors []OCPIConnectorInput, prices map[string]float64) []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range connectors {
		id := strings.TrimSpace(c.TariffID)
		if id == "" || seen[id] {
			continue
		}
		if _, ok := prices[id]; ok {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// tariffPricesTx reads every tariff of one partner as id → price inside the
// caller's transaction, so a location push prices its connectors in one query.
func tariffPricesTx(ctx context.Context, tx *sql.Tx, cpoID string) (map[string]float64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, price_per_kwh FROM tariffs WHERE cpo_id = $1`, cpoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]float64{}
	for rows.Next() {
		var id string
		var price float64
		if err := rows.Scan(&id, &price); err != nil {
			return nil, err
		}
		out[id] = price
	}
	return out, rows.Err()
}

func nullableJSON(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

// A newer unsupported tariff must not leave an old, chargeable price cached.
func (s *Store) InvalidateOCPITariff(ctx context.Context, cpoID, id string, raw []byte) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE tariffs SET pricing_supported=false,ocpi_json=$3::jsonb,last_updated=clock_timestamp() WHERE cpo_id=$1 AND id=$2`, cpoID, id, nullableJSON(raw))
	return err
}
