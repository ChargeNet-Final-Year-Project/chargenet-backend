package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/chargenet/chargenet-backend/internal/money"
	"strconv"
	"time"
)

// CDR is one settlement record: the money the platform
// earned, the CPO's share, and the OCPI-shaped document that describes the
// charge.
type CDR struct {
	ID               string         `json:"id"`
	SessionID        string         `json:"sessionId"`
	CpoID            string         `json:"cpoId"`
	UserID           string         `json:"userId"`
	TotalEnergyKWh   float64        `json:"totalEnergyKwh"`
	TotalTimeSeconds int            `json:"totalTimeSeconds"`
	GrossAmount      float64        `json:"grossAmount"`
	PlatformFee      float64        `json:"platformFee"`
	NetPayout        float64        `json:"netPayout"`
	Currency         string         `json:"currency"`
	Document         map[string]any `json:"cdrJson"`
	CreatedAt        time.Time      `json:"createdAt"`
}

// CDRStation is the station information an OCPI CDR location carries.
type CDRStation struct {
	OCPILocationID string
	CountryCode    string
	ID             string
	Name           string
	Address        string
	City           string
	Latitude       float64
	Longitude      float64
}

// SettleAmounts are the money facts of one settlement, computed by the store
// from the rows it locks so they can never disagree with what is committed.
type SettleAmounts struct {
	CDRID           string
	Currency        string
	EnergyKWh       float64
	DurationSeconds int
	TariffPerKWh    float64
	// Gross is the final cost: round2(kWh × tariff), capped at the wallet
	// balance so a final tick can never overdraw the driver.
	Gross          float64
	PlatformFeePct float64
	PlatformFee    float64
	NetPayout      float64
	StartTime      time.Time
	EndTime        time.Time
}

// CDRRequest is handed to SettleInput.BuildCDR inside the settlement
// transaction. Builders must stay pure — no database access — because the
// document has to describe exactly the rows the transaction commits.
type CDRRequest struct {
	Session *Session
	// Connector is the EVSE the charge ran on.
	Connector Connector
	// TariffID is the connector's tariff row (connectors.tariff_id), which the
	// OCPI tariff element of the CDR references.
	TariffID string
	// Station is the location the charge happened at.
	Station CDRStation
	// CPOCountryCode / CPOPartyID identify the CPO (OCPI country_code and
	// party_id of the operator that issued the CDR).
	CPOCountryCode string
	CPOPartyID     string
	Amounts        SettleAmounts
}

// SettleInput closes a charging session: it finalises the cost, debits the
// wallet, writes the CDR and frees the connector in one transaction.
type SettleInput struct {
	SessionID string
	// Status is COMPLETED (default) or FAILED.
	Status string
	// Reason is the stop reason recorded on the session.
	Reason string
	// State is the session's final telemetry.
	State SessionTelemetry
	// FallbackFeePct is used only when the CPO row has no platform_fee_pct.
	FallbackFeePct float64
	// BuildCDR renders the OCPI document; a nil builder stores an empty object.
	BuildCDR func(CDRRequest) ([]byte, error)
}

// SettleSession settles a session exactly once.
//
// A second call for an already-settled session is a no-op that returns the
// committed row, which is what makes `POST /sessions/{id}/stop` idempotent: the
// row is locked FOR UPDATE before the terminal status is inspected, so two
// concurrent stops cannot both debit the wallet.
func (s *Store) SettleSession(ctx context.Context, in SettleInput) (*Session, error) {
	energy, err := money.Product(3, strconv.FormatFloat(in.State.KWhDelivered, 'f', -1, 64))
	if err != nil {
		return nil, err
	}
	in.State.KWhDelivered = money.Float(energy)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var status, userID, cpoID, connectorID, stationID string
	var tariff float64
	err = tx.QueryRowContext(ctx,
		`SELECT status, user_id, cpo_id, connector_id, station_id, tariff_per_kwh
		   FROM charging_sessions WHERE id = $1 FOR UPDATE`, in.SessionID).
		Scan(&status, &userID, &cpoID, &connectorID, &stationID, &tariff)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if status != StatusPending && status != StatusCharging {
		return sessionTx(ctx, tx, in.SessionID) // already settled: keep it as it is
	}

	// Lock the wallet before examining held funds. Amounts are calculated in
	// PostgreSQL NUMERIC, including the explicit operator overrun policy.
	var balance string
	if err = tx.QueryRowContext(ctx, `SELECT balance FROM wallets WHERE user_id=$1 FOR UPDATE`, userID).Scan(&balance); err != nil {
		return nil, err
	}
	var original, converted, deduction, unfunded string
	var originalCurrency, walletCurrency, version, tariffID, countryCode, partyID string
	var feePct string
	err = tx.QueryRowContext(ctx, `SELECT s.original_currency,s.wallet_currency,coalesce(c.ocpi_version,'2.3.0'),
 coalesce(q.tariff_id,k.tariff_id,''),c.country_code,c.party_id,
 coalesce(c.platform_fee_pct,$2::numeric),round($3::numeric*s.tariff_per_kwh,2),
 round(round($3::numeric*s.tariff_per_kwh,2)*s.exchange_rate,2)
 FROM charging_sessions s JOIN cpo_partners c ON c.id=s.cpo_id
 JOIN connectors k ON k.id=s.connector_id LEFT JOIN charging_quotes q ON q.id=s.quote_id
 WHERE s.id=$1`, in.SessionID, strconv.FormatFloat(in.FallbackFeePct, 'f', 2, 64), energy).
		Scan(&originalCurrency, &walletCurrency, &version, &tariffID, &countryCode, &partyID, &feePct, &original, &converted)
	if err != nil {
		return nil, err
	}
	if in.State.KWhDelivered < 0 || in.State.DurationSeconds < 0 {
		return nil, fmt.Errorf("negative settlement telemetry")
	}
	// For quoted sessions the reservation is a spending ceiling. Any meter
	// overrun is retained as an explicit platform liability, never discarded
	// from the original CDR or charged beyond the accepted wallet budget.
	err = tx.QueryRowContext(ctx, `SELECT least($2::numeric,coalesce(r.amount,w.balance),w.balance),
 greatest(0,$2::numeric-least($2::numeric,coalesce(r.amount,w.balance),w.balance))
 FROM wallets w LEFT JOIN wallet_reservations r ON r.session_id=$1 AND r.status='HELD'
 WHERE w.user_id=$3`, in.SessionID, converted, userID).Scan(&deduction, &unfunded)
	if err != nil {
		return nil, err
	}
	var fee, net string
	err = tx.QueryRowContext(ctx, `SELECT round($1::numeric*$2::numeric/100,2),$1::numeric-round($1::numeric*$2::numeric/100,2)`, original, feePct).Scan(&fee, &net)
	if err != nil {
		return nil, err
	}
	amounts := SettleAmounts{CDRID: NewID("cdr"), Currency: originalCurrency,
		EnergyKWh: in.State.KWhDelivered, DurationSeconds: in.State.DurationSeconds, TariffPerKWh: tariff,
		Gross: money.Float(original), PlatformFeePct: money.Float(feePct), PlatformFee: money.Float(fee),
		NetPayout: money.Float(net), EndTime: time.Now().UTC()}

	sess, err := sessionTx(ctx, tx, in.SessionID)
	if err != nil {
		return nil, err
	}
	amounts.StartTime = sess.StartTime

	conn, err := connectorTx(ctx, tx, connectorID)
	if err != nil {
		return nil, err
	}
	station, err := cdrStationTx(ctx, tx, stationID)
	if err != nil {
		return nil, err
	}

	doc := []byte("{}")
	if in.BuildCDR != nil {
		doc, err = in.BuildCDR(CDRRequest{
			Session:        sess,
			Connector:      *conn,
			TariffID:       tariffID,
			Station:        *station,
			CPOCountryCode: countryCode,
			CPOPartyID:     partyID,
			Amounts:        amounts,
		})
		if err != nil {
			return nil, err
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO cdrs (id, session_id, cpo_id, user_id, total_energy_kwh, total_time_seconds,
		                   gross_amount, platform_fee, net_payout, currency, cdr_json, ocpi_version)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::jsonb, $12)`,
		amounts.CDRID, in.SessionID, cpoID, userID, amounts.EnergyKWh, amounts.DurationSeconds,
		original, fee, net, amounts.Currency, string(doc), version,
	); err != nil {
		return nil, err
	}

	if _, err = tx.ExecContext(ctx, `UPDATE wallet_reservations SET status='SETTLED',closed_at=now() WHERE session_id=$1 AND status='HELD'`, in.SessionID); err != nil {
		return nil, err
	}
	balanceAfter, err := debitWalletDecimal(ctx, tx, userID, deduction, amounts.CDRID)
	if err != nil {
		return nil, err
	}

	if _, err = tx.ExecContext(ctx, `UPDATE wallet_transactions SET session_id=$1,cdr_id=$2,currency=$3 WHERE reference_id=$2 AND type='CHARGING_DEBIT'`, in.SessionID, amounts.CDRID, walletCurrency); err != nil {
		return nil, err
	}
	finalStatus := in.Status
	if finalStatus == "" {
		finalStatus = StatusCompleted
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE charging_sessions
		    SET status = $2, end_time = NOW(), duration_seconds = $3, kwh_delivered = $4,
		        current_power_kw = $5, battery_soc = $6, total_cost = $7, balance_after = $8,
		        stop_reason = $9, original_cost=$10::numeric,unfunded_amount=$11::numeric
		  WHERE id = $1`,
		in.SessionID, finalStatus, in.State.DurationSeconds, in.State.KWhDelivered,
		in.State.CurrentPowerKW, in.State.BatterySoC, deduction, balanceAfter,
		nullString(in.Reason), original, unfunded,
	); err != nil {
		return nil, err
	}

	if err := setConnectorStatusTx(ctx, tx, connectorID, StatusAvailable); err != nil {
		return nil, err
	}

	settled, err := sessionTx(ctx, tx, in.SessionID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return settled, nil
}

// GetCDR loads one settlement record.
func (s *Store) GetCDR(ctx context.Context, id string) (*CDR, error) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT id, session_id, cpo_id, user_id, total_energy_kwh, total_time_seconds,
		        gross_amount, platform_fee, net_payout, currency, cdr_json, created_at
		   FROM cdrs WHERE id = $1`, id)
	c, err := scanCDR(row)
	if errors.Is(err, ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

func scanCDR(row rowScanner) (*CDR, error) {
	c := &CDR{}
	var raw []byte
	err := row.Scan(&c.ID, &c.SessionID, &c.CpoID, &c.UserID, &c.TotalEnergyKWh, &c.TotalTimeSeconds,
		&c.GrossAmount, &c.PlatformFee, &c.NetPayout, &c.Currency, &raw, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &c.Document); err != nil {
			return nil, fmt.Errorf("store: decode cdr json: %w", err)
		}
	}
	return c, nil
}

// sessionTx reads a session through the caller's transaction so the row it
// returns is the one the transaction is about to commit.
func sessionTx(ctx context.Context, tx *sql.Tx, id string) (*Session, error) {
	return scanSession(tx.QueryRowContext(ctx,
		`SELECT `+sessionColumns+sessionJoins+` WHERE s.id = $1`, id))
}

// connectorTx loads a connector inside the caller's transaction.
func connectorTx(ctx context.Context, tx *sql.Tx, id string) (*Connector, error) {
	c := &Connector{}
	err := tx.QueryRowContext(ctx,
		`SELECT id, station_id, evse_uid, standard, format, power_type, max_power_kw, tariff_per_kwh, status,coalesce(ocpi_connector_id,'')
           FROM connectors WHERE id = $1`, id).
		Scan(&c.ID, &c.StationID, &c.EVSEUID, &c.Standard, &c.Format, &c.PowerType,
			&c.MaxPowerKW, &c.TariffPerKWh, &c.Status, &c.OCPIConnectorID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

// cdrStationTx loads the station facts an OCPI CDR location carries.
func cdrStationTx(ctx context.Context, tx *sql.Tx, id string) (*CDRStation, error) {
	st := &CDRStation{}
	err := tx.QueryRowContext(ctx,
		`SELECT id, name, address, city, latitude, longitude,country_code,coalesce(ocpi_location_id,'') FROM stations WHERE id = $1`, id).
		Scan(&st.ID, &st.Name, &st.Address, &st.City, &st.Latitude, &st.Longitude, &st.CountryCode, &st.OCPILocationID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return st, nil
}

// nullString maps "" onto SQL NULL for nullable text columns.
func nullString(v string) sql.NullString {
	return sql.NullString{String: v, Valid: v != ""}
}
