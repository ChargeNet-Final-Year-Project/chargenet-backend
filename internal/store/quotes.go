package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

var (
	ErrQuoteRequired      = errors.New("review the estimated charging cost before starting")
	ErrQuoteConsumed      = errors.New("this estimate has already been used")
	ErrQuoteChanged       = errors.New("pricing changed; review the updated estimated total and confirm again")
	ErrRateUnavailable    = errors.New("a supported exchange rate is not available")
	ErrPricingUnsupported = errors.New("this charging rate is not supported by the energy-only prototype")
	ErrQuoteEnergy        = errors.New("estimatedKwh must be a decimal string from 0.001 to 250 with at most three decimal places")
)

var quoteEnergyPattern = regexp.MustCompile(`^[0-9]{1,3}(\.[0-9]{1,3})?$`)

// Decimal money values are strings in quotation/snapshot contracts. PostgreSQL
// NUMERIC does all authoritative arithmetic; JSON never rounds them via floats.
type ChargingQuote struct {
	ID                    string     `json:"id"`
	UserID                string     `json:"userId"`
	WalletID              string     `json:"walletId"`
	CpoID                 string     `json:"cpoId"`
	StationID             string     `json:"stationId"`
	ConnectorID           string     `json:"connectorId"`
	TariffID              string     `json:"tariffId"`
	TariffUpdatedAt       time.Time  `json:"tariffUpdatedAt"`
	TariffPerKWh          string     `json:"tariffPerKwh"`
	OriginalCurrency      string     `json:"originalCurrency"`
	WalletCurrency        string     `json:"walletCurrency"`
	ExchangeRateID        *string    `json:"exchangeRateId"`
	ExchangeRate          string     `json:"exchangeRate"`
	RateSource            string     `json:"rateSource"`
	EstimatedKWh          string     `json:"estimatedKwh"`
	EstimatedOriginalCost string     `json:"estimatedOriginalCost"`
	EstimatedWalletCost   string     `json:"estimatedWalletCost"`
	Budget                string     `json:"budget"`
	CreatedAt             time.Time  `json:"createdAt"`
	RateSelectionSource   string     `json:"-"`
	AcceptedAt            *time.Time `json:"acceptedAt"`
	EquivalentTariff      string     `json:"equivalentTariff"`
}

const quoteColumns = `q.id,q.user_id,q.wallet_id,q.cpo_id,q.station_id,q.connector_id,q.tariff_id,
 q.tariff_updated_at,q.tariff_per_kwh,q.original_currency,q.wallet_currency,q.exchange_rate_id,
 q.exchange_rate,q.rate_source,q.estimated_kwh,q.estimated_original_cost,q.estimated_wallet_cost,
 q.budget,q.created_at,q.rate_selection_source,q.accepted_at,round(q.tariff_per_kwh*q.exchange_rate,6)`

func scanQuote(row rowScanner) (*ChargingQuote, error) {
	q := &ChargingQuote{}
	err := row.Scan(&q.ID, &q.UserID, &q.WalletID, &q.CpoID, &q.StationID, &q.ConnectorID, &q.TariffID,
		&q.TariffUpdatedAt, &q.TariffPerKWh, &q.OriginalCurrency, &q.WalletCurrency, &q.ExchangeRateID,
		&q.ExchangeRate, &q.RateSource, &q.EstimatedKWh, &q.EstimatedOriginalCost, &q.EstimatedWalletCost,
		&q.Budget, &q.CreatedAt, &q.RateSelectionSource, &q.AcceptedAt, &q.EquivalentTariff)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return q, err
}

func (s *Store) GetQuote(ctx context.Context, userID, id string) (*ChargingQuote, error) {
	return scanQuote(s.DB.QueryRowContext(ctx, `SELECT `+quoteColumns+` FROM charging_quotes q WHERE q.id=$1 AND q.user_id=$2`, id, userID))
}

func (s *Store) CreateQuote(ctx context.Context, userID, evse, estimatedKWh string, sources ...string) (*ChargingQuote, error) {
	if estimatedKWh == "" {
		estimatedKWh = "15"
	}
	value, parseErr := strconv.ParseFloat(estimatedKWh, 64)
	if !quoteEnergyPattern.MatchString(estimatedKWh) || parseErr != nil || value <= 0 || value > 250 {
		return nil, ErrQuoteEnergy
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var walletID, walletCurrency string
	var funds bool
	err = tx.QueryRowContext(ctx, `SELECT w.id,w.currency,
 w.balance-coalesce((SELECT sum(amount) FROM wallet_reservations WHERE wallet_id=w.id AND status='HELD'),0)>0
 FROM wallets w WHERE user_id=$1 FOR UPDATE`, userID).Scan(&walletID, &walletCurrency, &funds)
	if err != nil {
		return nil, err
	}
	if !funds {
		return nil, ErrInsufficientBalance
	}
	var connID, stationID, cpoID, tariffID, currency, status, cpoStatus string
	var supported, active bool
	err = tx.QueryRowContext(ctx, `SELECT k.id,st.id,c.id,t.id,t.currency,k.status,c.status,t.pricing_supported,st.is_active
 FROM connectors k JOIN stations st ON st.id=k.station_id JOIN cpo_partners c ON c.id=st.cpo_id
 JOIN tariffs t ON t.id=k.tariff_id AND t.cpo_id=c.id WHERE k.evse_uid=$1 FOR SHARE OF k,st,c,t`, evse).
		Scan(&connID, &stationID, &cpoID, &tariffID, &currency, &status, &cpoStatus, &supported, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if status != StatusAvailable || cpoStatus != CPOStatusActive || !active {
		return nil, ErrConnectorBusy
	}
	if !supported {
		return nil, ErrPricingUnsupported
	}
	if !ValidCurrency(currency) || !ValidCurrency(walletCurrency) {
		return nil, ErrRateUnavailable
	}
	selectedSource := ""
	if len(sources) > 0 {
		selectedSource = sources[0]
	}
	var rateID any
	rate, source := "1", "same-currency"
	if currency != walletCurrency {
		var id string
		err = tx.QueryRowContext(ctx, `SELECT id,rate,source FROM exchange_rates WHERE source_currency=$1 AND target_currency=$2 AND effective_at<=now() AND ($3='' OR source=$3) ORDER BY effective_at DESC,id DESC LIMIT 1`, currency, walletCurrency, selectedSource).Scan(&id, &rate, &source)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRateUnavailable
		}
		if err != nil {
			return nil, err
		}
		rateID = id
	}
	id := NewID("qte")
	res, err := tx.ExecContext(ctx, `INSERT INTO charging_quotes(id,user_id,wallet_id,cpo_id,station_id,connector_id,tariff_id,
 tariff_updated_at,tariff_per_kwh,original_currency,wallet_currency,exchange_rate_id,exchange_rate,rate_source,
 estimated_kwh,estimated_original_cost,estimated_wallet_cost,budget,rate_selection_source)
 SELECT $1,$2,w.id,$3,$4,$5,t.id,t.last_updated,t.price_per_kwh,t.currency,w.currency,$7,$8::numeric,$9,
 $10::numeric,round($10::numeric*t.price_per_kwh,2),round(round($10::numeric*t.price_per_kwh,2)*$8::numeric,2),
 w.balance-coalesce((SELECT sum(amount) FROM wallet_reservations WHERE wallet_id=w.id AND status='HELD'),0),
 $11
 FROM tariffs t, wallets w WHERE t.id=$6 AND w.id=$12 AND t.price_per_kwh>0
 AND $10::numeric BETWEEN 0.001 AND 250`, id, userID, cpoID, stationID, connID, tariffID, rateID, rate, source, estimatedKWh, selectedSource, walletID)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, ErrPricingUnsupported
	}
	q, err := scanQuote(tx.QueryRowContext(ctx, `SELECT `+quoteColumns+` FROM charging_quotes q WHERE id=$1`, id))
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return q, nil
}

func ValidCurrency(c string) bool { return c == "MYR" || c == "SGD" || c == "THB" }

// acceptQuoteTx uses the same wallet-first lock order as all wallet mutations.
// Pricing, availability, quote consumption, reservation and session insertion
// commit together. A failed validation rolls all of them back.
func acceptQuoteTx(ctx context.Context, tx *sql.Tx, in PendingSessionInput, sessionID string) (*ChargingQuote, error) {
	var walletID, currency string
	var balance string
	if err := tx.QueryRowContext(ctx, `SELECT id,currency,balance FROM wallets WHERE user_id=$1 FOR UPDATE`, in.UserID).Scan(&walletID, &currency, &balance); err != nil {
		return nil, err
	}
	q, err := scanQuote(tx.QueryRowContext(ctx, `SELECT `+quoteColumns+` FROM charging_quotes q WHERE id=$1 AND user_id=$2 FOR UPDATE`, in.QuoteID, in.UserID))
	if err != nil {
		return nil, err
	}
	if q.AcceptedAt != nil {
		return nil, ErrQuoteConsumed
	}
	var valid bool
	if q.WalletID != walletID || q.WalletCurrency != currency || q.CpoID != in.CPOID || q.StationID != in.StationID || q.ConnectorID != in.ConnectorID {
		return nil, ErrQuoteChanged
	}
	var available, pricingValid bool
	err = tx.QueryRowContext(ctx, `SELECT k.status='AVAILABLE' AND st.is_active AND c.status='ACTIVE',
 t.id=$2 AND t.currency=$3 AND t.price_per_kwh=$4::numeric AND t.last_updated=$5 AND t.pricing_supported AND t.cpo_id=c.id
 FROM connectors k JOIN stations st ON st.id=k.station_id JOIN cpo_partners c ON c.id=st.cpo_id
 JOIN tariffs t ON t.id=k.tariff_id WHERE k.id=$1 FOR UPDATE OF k,st,c,t`, q.ConnectorID, q.TariffID, q.OriginalCurrency, q.TariffPerKWh, q.TariffUpdatedAt).Scan(&available, &pricingValid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrQuoteChanged
	}
	if err != nil {
		return nil, err
	}
	if !available {
		return nil, ErrConnectorBusy
	}
	if !pricingValid {
		return nil, ErrQuoteChanged
	}
	if q.OriginalCurrency != q.WalletCurrency {
		// Serialise appends through the acceptance commit. A row lock alone
		// would not stop a newer rate being inserted after this lookup.
		if _, err = tx.ExecContext(ctx, `LOCK TABLE exchange_rates IN SHARE MODE`); err != nil {
			return nil, err
		}
		var id, rate, source string
		var unchanged bool
		err = tx.QueryRowContext(ctx, `SELECT id,rate,source,rate=$4::numeric FROM exchange_rates
          WHERE source_currency=$1 AND target_currency=$2 AND effective_at<=clock_timestamp()
          AND ($3='' OR source=$3) ORDER BY effective_at DESC,id DESC LIMIT 1`,
			q.OriginalCurrency, q.WalletCurrency, q.RateSelectionSource, q.ExchangeRate).Scan(&id, &rate, &source, &unchanged)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRateUnavailable
		}
		if err != nil {
			return nil, err
		}
		if !unchanged {
			return nil, ErrQuoteChanged
		}
		q.ExchangeRateID, q.ExchangeRate, q.RateSource = &id, rate, source
	}
	err = tx.QueryRowContext(ctx, `SELECT $1::numeric <= balance-coalesce((SELECT sum(amount) FROM wallet_reservations WHERE wallet_id=w.id AND status='HELD'),0) FROM wallets w WHERE id=$2`, q.Budget, walletID).Scan(&valid)
	if err != nil {
		return nil, err
	}
	if !valid {
		return nil, ErrInsufficientBalance
	}
	if _, err = tx.ExecContext(ctx, `UPDATE charging_quotes SET accepted_at=clock_timestamp(), exchange_rate_id=$2,rate_source=$3 WHERE id=$1`, q.ID, q.ExchangeRateID, q.RateSource); err != nil {
		return nil, err
	}
	return q, nil
}

// AddExchangeRate appends a version; accepted quotations always retain theirs.
func (s *Store) AddExchangeRate(ctx context.Context, from, to, rate, source string, effective time.Time) error {
	if !ValidCurrency(from) || !ValidCurrency(to) || source == "" {
		return fmt.Errorf("invalid exchange rate metadata")
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO exchange_rates(id,source_currency,target_currency,rate,effective_at,source) VALUES($1,$2,$3,$4::numeric,$5,$6)`, NewID("fx"), from, to, rate, effective, source)
	return err
}
