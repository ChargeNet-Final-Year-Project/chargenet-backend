package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Session-level failures. The orchestrator (internal/sessions) maps them onto
// the API codes of the API contract
var (
	// ErrActiveSession means the driver already has a PENDING or CHARGING
	// session, or another driver grabbed the connector first.
	ErrActiveSession = errors.New("store: an active session already exists")
	// ErrConnectorBusy means the connector is not AVAILABLE.
	ErrConnectorBusy = errors.New("store: connector is not available")
)

// CurrencyMYR is the legacy/default wallet denomination.
const CurrencyMYR = "MYR"

// Session is the public ChargingSession DTO. Its JSON tags
// are the exact shape the mobile `ChargingSession` model parses, so the
// orchestrator can stream it straight onto an SSE frame.
type Session struct {
	SettlementRequestedStatus *string `json:"-"`
	SettlementRequestedReason *string `json:"-"`
	QuoteID                   *string `json:"quoteId"`
	OriginalCurrency          string  `json:"originalCurrency"`
	WalletCurrency            string  `json:"walletCurrency"`
	ExchangeRate              string  `json:"exchangeRate"`
	RateSource                string  `json:"rateSource"`
	OriginalCost              float64 `json:"originalCost"`
	UnfundedAmount            float64 `json:"unfundedAmount"`
	ReservedAmount            float64 `json:"reservedAmount"`
	TariffDecimal             string  `json:"tariffDecimal"`

	ID              string     `json:"id"`
	UserID          string     `json:"userId"`
	VehicleID       *string    `json:"vehicleId"`
	CpoID           string     `json:"cpoId"`
	CpoName         string     `json:"cpoName"`
	StationID       string     `json:"stationId"`
	StationName     string     `json:"stationName"`
	ConnectorID     string     `json:"connectorId"`
	EVSEUID         string     `json:"evseUid"`
	ConnectorType   string     `json:"connectorType"`
	Status          string     `json:"status"`
	StartTime       time.Time  `json:"startTime"`
	EndTime         *time.Time `json:"endTime"`
	DurationSeconds int        `json:"durationSeconds"`
	KWhDelivered    float64    `json:"kwhDelivered"`
	CurrentPowerKW  float64    `json:"currentPowerKw"`
	BatterySoC      float64    `json:"batterySoc"`
	TargetSoC       float64    `json:"targetSoc"`
	TariffPerKWh    float64    `json:"tariffPerKwh"`
	TotalCost       float64    `json:"totalCost"`
	BalanceAfter    *float64   `json:"balanceAfter"`
	StopReason      *string    `json:"stopReason"`
	CDRID           *string    `json:"cdrId"`
	// CDRJSON is the OCPI CDR document (an object, never a string); it is only
	// populated once the session has been settled.
	CDRJSON map[string]any `json:"cdrJson"`

	// OCPISessionID is the CPO's own session id. It is internal to the
	// platform and deliberately not part of the mobile DTO.
	OCPISessionID *string `json:"-"`
}

// Terminal reports whether a session can never change again.
func (s Session) Terminal() bool {
	return s.Status == StatusCompleted || s.Status == StatusFailed
}

// Active reports whether a session still holds its connector.
func (s Session) Active() bool {
	return s.Status == StatusPending || s.Status == StatusCharging
}

// SessionTelemetry is the live snapshot the orchestrator persists on its
// throttled tick and settles with.
type SessionTelemetry struct {
	DurationSeconds int
	KWhDelivered    float64
	CurrentPowerKW  float64
	BatterySoC      float64
	TotalCost       float64
}

// PendingSessionInput opens a session row before the CPO has accepted it.
type PendingSessionInput struct {
	QuoteID        string
	UserID         string
	VehicleID      string // "" when the driver has no vehicle on file
	CPOID          string
	StationID      string
	ConnectorID    string
	EVSEUID        string
	StartSoC       float64
	CurrentPowerKW float64
	TargetSoC      float64
	TariffPerKWh   float64
}

// sessionColumns / sessionJoins is the projection every session read shares.
// cdr_json comes from the CDR the settlement wrote, never from the session row.
const sessionColumns = `s.id, s.user_id, s.vehicle_id, s.cpo_id, c.name, s.station_id, st.name,
	s.connector_id, s.evse_uid, k.standard, s.status, s.start_time, s.end_time,
	s.duration_seconds, s.kwh_delivered, s.current_power_kw, s.battery_soc, s.target_soc,
	s.tariff_per_kwh, s.total_cost, s.balance_after, s.stop_reason, d.id, d.cdr_json,
	s.ocpi_session_id,s.quote_id,s.original_currency,s.wallet_currency,s.exchange_rate,s.rate_source,
 s.original_cost,s.unfunded_amount,coalesce(r.amount,0),s.tariff_per_kwh,s.settlement_requested_status,s.settlement_requested_reason`

const sessionJoins = `
	  FROM charging_sessions s
	  JOIN cpo_partners c ON c.id = s.cpo_id
	  JOIN stations st ON st.id = s.station_id
	  JOIN connectors k ON k.id = s.connector_id
	  LEFT JOIN cdrs d ON d.session_id = s.id
 LEFT JOIN wallet_reservations r ON r.session_id=s.id`

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanSession reads one row of sessionColumns.
func scanSession(row rowScanner) (*Session, error) {
	s := &Session{}
	var vehicleID, stopReason, cdrID, ocpiID sql.NullString
	var endTime sql.NullTime
	var balanceAfter sql.NullFloat64
	var cdrRaw []byte

	err := row.Scan(&s.ID, &s.UserID, &vehicleID, &s.CpoID, &s.CpoName, &s.StationID, &s.StationName,
		&s.ConnectorID, &s.EVSEUID, &s.ConnectorType, &s.Status, &s.StartTime, &endTime,
		&s.DurationSeconds, &s.KWhDelivered, &s.CurrentPowerKW, &s.BatterySoC, &s.TargetSoC,
		&s.TariffPerKWh, &s.TotalCost, &balanceAfter, &stopReason, &cdrID, &cdrRaw, &ocpiID, &s.QuoteID, &s.OriginalCurrency, &s.WalletCurrency, &s.ExchangeRate, &s.RateSource, &s.OriginalCost, &s.UnfundedAmount, &s.ReservedAmount, &s.TariffDecimal, &s.SettlementRequestedStatus, &s.SettlementRequestedReason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	s.VehicleID = nullStringPtr(vehicleID)
	s.EndTime = nullTimePtr(endTime)
	s.BalanceAfter = nullFloatPtr(balanceAfter)
	s.StopReason = nullStringPtr(stopReason)
	s.CDRID = nullStringPtr(cdrID)
	s.OCPISessionID = nullStringPtr(ocpiID)

	if len(cdrRaw) > 0 {
		var doc map[string]any
		if err := json.Unmarshal(cdrRaw, &doc); err != nil {
			return nil, fmt.Errorf("store: decode cdr json: %w", err)
		}
		s.CDRJSON = doc
	}
	return s, nil
}

// GetSession loads one of the caller's own sessions.
func (s *Store) GetSession(ctx context.Context, userID, id string) (*Session, error) {
	return scanSession(s.DB.QueryRowContext(ctx,
		`SELECT `+sessionColumns+sessionJoins+`
		  WHERE s.id = $1 AND s.user_id = $2`, id, userID))
}

// SessionByID loads a session without an owner check: the orchestrator's (and
// OCPI receivers's receiver's) internal read.
func (s *Store) SessionByID(ctx context.Context, id string) (*Session, error) {
	return scanSession(s.DB.QueryRowContext(ctx,
		`SELECT `+sessionColumns+sessionJoins+`
		  WHERE s.id = $1`, id))
}

// GetActiveSession returns the driver's running (or starting) session.
func (s *Store) GetActiveSession(ctx context.Context, userID string) (*Session, error) {
	return scanSession(s.DB.QueryRowContext(ctx,
		`SELECT `+sessionColumns+sessionJoins+`
		  WHERE s.user_id = $1 AND s.status IN ('PENDING', 'CHARGING')
		  ORDER BY s.start_time DESC LIMIT 1`, userID))
}

// ListSessionHistory returns the driver's finished sessions, newest first.
func (s *Store) ListSessionHistory(ctx context.Context, userID string, limit, offset int) ([]Session, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT `+sessionColumns+sessionJoins+`
		  WHERE s.user_id = $1 AND s.status IN ('COMPLETED', 'FAILED')
		  ORDER BY s.start_time DESC, s.id DESC
		  LIMIT $2 OFFSET $3`, userID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Session{}
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sess)
	}
	return out, rows.Err()
}

// ResumableSession is a PENDING or CHARGING session together with the connector
// and vehicle facts the orchestrator needs to restart its simulation after a
// backend restart.
type ResumableSession struct {
	Session   *Session
	Connector *Connector
	Vehicle   *Vehicle // nil when the driver has no vehicle on file
}

// ListResumableSessions returns every session that is still holding a connector
// (PENDING or CHARGING) so a restarting backend can pick it up.
func (s *Store) ListResumableSessions(ctx context.Context) ([]ResumableSession, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT `+sessionColumns+sessionJoins+`
		  WHERE s.status IN ('PENDING', 'CHARGING')
		  ORDER BY s.start_time ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []*Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(sessions) == 0 {
		return nil, nil
	}

	connIDs := make([]string, 0, len(sessions))
	vehIDs := make([]string, 0, len(sessions))
	for _, sess := range sessions {
		connIDs = append(connIDs, sess.ConnectorID)
		if sess.VehicleID != nil {
			vehIDs = append(vehIDs, *sess.VehicleID)
		}
	}
	conns, err := s.connectorsByID(ctx, connIDs)
	if err != nil {
		return nil, err
	}
	vehs, err := s.vehiclesByID(ctx, vehIDs)
	if err != nil {
		return nil, err
	}

	out := make([]ResumableSession, 0, len(sessions))
	for _, sess := range sessions {
		r := ResumableSession{Session: sess, Connector: conns[sess.ConnectorID]}
		if sess.VehicleID != nil {
			r.Vehicle = vehs[*sess.VehicleID]
		}
		out = append(out, r)
	}
	return out, nil
}

// InsertPendingSession runs the storage half of ordered start
// validation and opens the session row. Racing starts are resolved by the
// partial unique indexes (one_active_session_per_user and
// one_active_session_per_connector) guarantee that racing starts create one row.
func (s *Store) InsertPendingSession(ctx context.Context, in PendingSessionInput) (*Session, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	id := NewID("ses")
	var quote *ChargingQuote
	if in.QuoteID != "" {
		quote, err = acceptQuoteTx(ctx, tx, in, id)
		if err != nil {
			return nil, err
		}
	}
	var existing string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM charging_sessions
		  WHERE user_id = $1 AND status IN ('PENDING', 'CHARGING') LIMIT 1`, in.UserID).Scan(&existing)
	switch {
	case err == nil:
		return nil, ErrActiveSession
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}

	var status string
	err = tx.QueryRowContext(ctx,
		`SELECT status FROM connectors WHERE id = $1`, in.ConnectorID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if status != StatusAvailable {
		return nil, ErrConnectorBusy
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO charging_sessions
		   (id, user_id, vehicle_id, cpo_id, station_id, connector_id, evse_uid, status,
		    start_time, duration_seconds, kwh_delivered, current_power_kw, battery_soc,
		    target_soc, tariff_per_kwh, total_cost)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, 'PENDING', NOW(), 0, 0, $8, $9, $10, $11, 0)`,
		id, in.UserID, nullString(in.VehicleID), in.CPOID, in.StationID, in.ConnectorID,
		in.EVSEUID, in.CurrentPowerKW, in.StartSoC, int(in.TargetSoC), in.TariffPerKWh)
	if err != nil {
		switch {
		case isUniqueViolation(err, "one_active_session_per_user"):
			return nil, ErrActiveSession
		case isUniqueViolation(err, "one_active_session_per_connector"):
			return nil, ErrConnectorBusy
		}
		return nil, err
	}
	if quote != nil {
		_, err = tx.ExecContext(ctx, `UPDATE charging_sessions SET quote_id=$2,tariff_per_kwh=$3::numeric,
  original_currency=$4,wallet_currency=$5,exchange_rate=$6::numeric,rate_source=$7 WHERE id=$1`,
			id, quote.ID, quote.TariffPerKWh, quote.OriginalCurrency, quote.WalletCurrency, quote.ExchangeRate, quote.RateSource)
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO wallet_reservations(session_id,wallet_id,amount) VALUES($1,$2,$3::numeric)`, id, quote.WalletID, quote.Budget)
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.SessionByID(ctx, id)
}

// ActivateSession flips a PENDING session (and its connector) to CHARGING once
// the gateway has accepted it, in a single transaction.
func (s *Store) ActivateSession(ctx context.Context, sessionID string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var connectorID string
	err = tx.QueryRowContext(ctx,
		`UPDATE charging_sessions SET status = 'CHARGING' WHERE id = $1 AND status = 'PENDING'
		 RETURNING connector_id`, sessionID).Scan(&connectorID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := setConnectorStatusTx(ctx, tx, connectorID, StatusCharging); err != nil {
		return err
	}
	return tx.Commit()
}

// FailSession marks a session FAILED without settling it: it is used when the
// CPO never accepted the charge (or a start died mid-flight), so there is
// nothing to bill and the connector must simply become AVAILABLE again.
func (s *Store) FailSession(ctx context.Context, sessionID, reason string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var connectorID string
	err = tx.QueryRowContext(ctx,
		`UPDATE charging_sessions
		    SET status = 'FAILED', end_time = NOW(), stop_reason = $2
		  WHERE id = $1 AND status IN ('PENDING', 'CHARGING')
		 RETURNING connector_id`, sessionID, reason).Scan(&connectorID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE wallet_reservations SET status='RELEASED',closed_at=now() WHERE session_id=$1 AND status='HELD'`, sessionID); err != nil {
		return err
	}
	if err := setConnectorStatusTx(ctx, tx, connectorID, StatusAvailable); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateSessionTelemetry persists the throttled live numbers of a running
// session. Settled sessions are never touched: the WHERE clause makes the write
// a no-op once the terminal event has been settled, so a late tick cannot
// resurrect a finished session.
func (s *Store) UpdateSessionTelemetry(ctx context.Context, sessionID string, t SessionTelemetry) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE charging_sessions
		    SET duration_seconds = $2, kwh_delivered = $3, current_power_kw = $4,
		        battery_soc = $5,
                original_cost=round(round($3::numeric,3)*tariff_per_kwh,2),
                total_cost=round(round(round($3::numeric,3)*tariff_per_kwh,2)*exchange_rate,2)
		  WHERE id = $1 AND status IN ('PENDING', 'CHARGING')`,
		sessionID, t.DurationSeconds, t.KWhDelivered, t.CurrentPowerKW, t.BatterySoC)
	return err
}

// connectorsByID loads connectors keyed by id.
func (s *Store) connectorsByID(ctx context.Context, ids []string) (map[string]*Connector, error) {
	out := map[string]*Connector{}
	if len(ids) == 0 {
		return out, nil
	}
	args, parts := sqlIDList(ids)
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, station_id, evse_uid, standard, format, power_type, max_power_kw, tariff_per_kwh, status
		   FROM connectors WHERE id IN (`+strings.Join(parts, ", ")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		c := &Connector{}
		if err := rows.Scan(&c.ID, &c.StationID, &c.EVSEUID, &c.Standard, &c.Format,
			&c.PowerType, &c.MaxPowerKW, &c.TariffPerKWh, &c.Status); err != nil {
			return nil, err
		}
		out[c.ID] = c
	}
	return out, rows.Err()
}

// vehiclesByID loads vehicles keyed by id.
func (s *Store) vehiclesByID(ctx context.Context, ids []string) (map[string]*Vehicle, error) {
	out := map[string]*Vehicle{}
	if len(ids) == 0 {
		return out, nil
	}
	args, parts := sqlIDList(ids)
	rows, err := s.DB.QueryContext(ctx,
		`SELECT `+vehicleColumns+` FROM vehicles WHERE id IN (`+strings.Join(parts, ", ")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		v := &Vehicle{}
		if err := rows.Scan(&v.ID, &v.UserID, &v.Name, &v.LicensePlate,
			&v.ConnectorType, &v.BatteryCapacity, &v.IsDefault); err != nil {
			return nil, err
		}
		out[v.ID] = v
	}
	return out, rows.Err()
}

// sqlIDList renders a `$1, $2, …` placeholder list for an IN clause, together
// with its arguments (pgx does not accept a bare []string here).
func sqlIDList(ids []string) ([]any, []string) {
	args := make([]any, len(ids))
	parts := make([]string, len(ids))
	for i, id := range ids {
		args[i] = id
		parts[i] = fmt.Sprintf("$%d", i+1)
	}
	return args, parts
}

func nullStringPtr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}

func nullTimePtr(v sql.NullTime) *time.Time {
	if !v.Valid {
		return nil
	}
	t := v.Time
	return &t
}

func nullFloatPtr(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	f := v.Float64
	return &f
}
