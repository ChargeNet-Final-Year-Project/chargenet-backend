package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
	_ "time/tzdata"
)

// ReportingTimezone is the approved calendar boundary for Malaysia/MYR reports.
const ReportingTimezone = "Asia/Kuala_Lumpur"

var ReportingLocation = func() *time.Location {
	location, err := time.LoadLocation(ReportingTimezone)
	if err != nil {
		panic(err)
	}
	return location
}()

// PortalStation is one station of a CPO as the console's network view renders
// it: the station's own fields plus every bay under it.
type PortalStation struct {
	CountryCode   string      `json:"countryCode"`
	IsSimulated   bool        `json:"isSimulated"`
	ID            string      `json:"id"`
	Name          string      `json:"name"`
	Address       string      `json:"address"`
	City          string      `json:"city"`
	Latitude      float64     `json:"latitude"`
	Longitude     float64     `json:"longitude"`
	TotalBays     int         `json:"totalBays"`
	AvailableBays int         `json:"availableBays"`
	Connectors    []Connector `json:"connectors"`
}

// ConnectorCounts summarises a CPO's bays by status.
type ConnectorCounts struct {
	Available   int `json:"available"`
	Charging    int `json:"charging"`
	Blocked     int `json:"blocked"`
	Inoperative int `json:"inoperative"`
}

// NetworkView is `GET /admin/api/network`: the CPO's stations with live bay
// status, the status counts and the day's session count.
type NetworkView struct {
	Stations      []PortalStation `json:"stations"`
	Counts        ConnectorCounts `json:"counts"`
	SessionsToday int             `json:"sessionsToday"`
}

// NetworkForCPO reads a CPO's whole network. Unlike the driver's discovery
// (which filters to ACTIVE partners and active stations) this is the operator's
// own view, so a PENDING partner can watch its bays before it is live.
func (s *Store) NetworkForCPO(ctx context.Context, cpoID string) (*NetworkView, error) {
	out := &NetworkView{Stations: []PortalStation{}}

	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, name, address, city, latitude, longitude, total_bays, available_bays,country_code,is_simulated
		   FROM stations WHERE cpo_id = $1 ORDER BY name`, cpoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only result set

	index := map[string]int{}
	for rows.Next() {
		var st PortalStation
		if err := rows.Scan(&st.ID, &st.Name, &st.Address, &st.City,
			&st.Latitude, &st.Longitude, &st.TotalBays, &st.AvailableBays, &st.CountryCode, &st.IsSimulated); err != nil {
			return nil, err
		}
		st.Connectors = []Connector{}
		index[st.ID] = len(out.Stations)
		out.Stations = append(out.Stations, st)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	crows, err := s.DB.QueryContext(ctx,
		`SELECT k.id, k.station_id, k.evse_uid, k.standard, k.format, k.power_type,
		        k.max_power_kw, k.tariff_per_kwh, k.status,coalesce((SELECT currency FROM tariffs WHERE id=k.tariff_id),'MYR'),k.tariff_id
		   FROM connectors k JOIN stations st ON st.id = k.station_id
		  WHERE st.cpo_id = $1 ORDER BY st.name, k.evse_uid`, cpoID)
	if err != nil {
		return nil, err
	}
	defer crows.Close() //nolint:errcheck // read-only result set

	for crows.Next() {
		var c Connector
		if err := crows.Scan(&c.ID, &c.StationID, &c.EVSEUID, &c.Standard, &c.Format,
			&c.PowerType, &c.MaxPowerKW, &c.TariffPerKWh, &c.Status, &c.Currency, &c.TariffID); err != nil {
			return nil, err
		}
		if i, ok := index[c.StationID]; ok {
			out.Stations[i].Connectors = append(out.Stations[i].Connectors, c)
		}
		countConnector(&out.Counts, c.Status)
	}
	if err := crows.Err(); err != nil {
		return nil, err
	}

	// The sandbox driver's sessions are excluded everywhere money is reported
	//, but a live count is not money, so it is included.
	if err := s.DB.QueryRowContext(ctx,
		`SELECT count(*) FROM charging_sessions
		  WHERE cpo_id = $1 AND start_time >= (date_trunc('day', now() AT TIME ZONE 'Asia/Kuala_Lumpur') AT TIME ZONE 'Asia/Kuala_Lumpur') AND start_time < ((date_trunc('day', now() AT TIME ZONE 'Asia/Kuala_Lumpur') + interval '1 day') AT TIME ZONE 'Asia/Kuala_Lumpur')`,
		cpoID).Scan(&out.SessionsToday); err != nil {
		return nil, err
	}
	return out, nil
}

// countConnector folds one bay status into the counters.
func countConnector(c *ConnectorCounts, status string) {
	switch status {
	case StatusAvailable:
		c.Available++
	case StatusCharging:
		c.Charging++
	case StatusBlocked:
		c.Blocked++
	case StatusInoperative:
		c.Inoperative++
	}
}

// DailyPoint is one day of the 7-day chart (sessions and gross).
type DailyPoint struct {
	Day      string  `json:"day"`
	Sessions int     `json:"sessions"`
	Gross    float64 `json:"gross"`
}

// Overview is `GET /admin/api/overview`: the KPI cards, the bay-status donut and
// the 7-day chart for one CPO.
type Overview struct {
	Currency       string          `json:"currency"`
	SessionsToday  int             `json:"sessionsToday"`
	KWhToday       float64         `json:"kwhToday"`
	GrossToday     float64         `json:"grossToday"`
	ActiveSessions int             `json:"activeSessions"`
	NetPayoutMTD   float64         `json:"netPayoutMtd"`
	Counts         ConnectorCounts `json:"counts"`
	Last7Days      []DailyPoint    `json:"last7Days"`
}

// OverviewForCPO computes one CPO's dashboard. Money comes from the CDR ledger
// (the settled truth) and excludes the sandbox driver; live counts come from the
// session rows.
func (s *Store) OverviewForCPO(ctx context.Context, cpoID string, currencies ...string) (*Overview, error) {
	out := &Overview{Last7Days: []DailyPoint{}, Currency: reportCurrency(currencies)}

	if err := s.DB.QueryRowContext(ctx,
		`SELECT count(*), COALESCE(sum(kwh_delivered), 0)
		   FROM charging_sessions
		  WHERE cpo_id = $1 AND user_id <> $2 AND start_time >= (date_trunc('day', now() AT TIME ZONE 'Asia/Kuala_Lumpur') AT TIME ZONE 'Asia/Kuala_Lumpur') AND start_time < ((date_trunc('day', now() AT TIME ZONE 'Asia/Kuala_Lumpur') + interval '1 day') AT TIME ZONE 'Asia/Kuala_Lumpur')`,
		cpoID, SandboxUserID).Scan(&out.SessionsToday, &out.KWhToday); err != nil {
		return nil, err
	}
	if err := s.DB.QueryRowContext(ctx,
		`SELECT count(*) FROM charging_sessions
		  WHERE cpo_id = $1 AND status IN ('PENDING', 'CHARGING')`,
		cpoID).Scan(&out.ActiveSessions); err != nil {
		return nil, err
	}
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COALESCE(sum(gross_amount), 0) FROM cdrs
		  WHERE currency='`+out.Currency+`' AND cpo_id = $1 AND user_id <> $2 AND created_at >= (date_trunc('day', now() AT TIME ZONE 'Asia/Kuala_Lumpur') AT TIME ZONE 'Asia/Kuala_Lumpur') AND created_at < ((date_trunc('day', now() AT TIME ZONE 'Asia/Kuala_Lumpur') + interval '1 day') AT TIME ZONE 'Asia/Kuala_Lumpur')`,
		cpoID, SandboxUserID).Scan(&out.GrossToday); err != nil {
		return nil, err
	}
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COALESCE(sum(net_payout), 0) FROM cdrs
		  WHERE currency='`+out.Currency+`' AND cpo_id = $1 AND user_id <> $2 AND created_at >= (date_trunc('month', now() AT TIME ZONE 'Asia/Kuala_Lumpur') AT TIME ZONE 'Asia/Kuala_Lumpur') AND created_at < ((date_trunc('day', now() AT TIME ZONE 'Asia/Kuala_Lumpur') + interval '1 day') AT TIME ZONE 'Asia/Kuala_Lumpur')`,
		cpoID, SandboxUserID).Scan(&out.NetPayoutMTD); err != nil {
		return nil, err
	}

	// Reuse the network query so bay status counts stay consistent.
	rows, err := s.DB.QueryContext(ctx,
		`SELECT k.status, count(*)
		   FROM connectors k JOIN stations st ON st.id = k.station_id
		  WHERE st.cpo_id = $1 GROUP BY k.status`, cpoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only result set
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		for i := 0; i < n; i++ {
			countConnector(&out.Counts, status)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	daily, err := s.dailySeries(ctx, `c.cpo_id = $1 AND c.user_id <> $2 AND c.currency='`+out.Currency+`'`, cpoID, SandboxUserID)
	if err != nil {
		return nil, err
	}
	out.Last7Days = daily
	return out, nil
}

// dailySeries returns the last seven days of CDR count and gross, one point per
// day with the empty days filled in, so a chart never has to guess at a gap.
// The predicate is parameterised so the CPO and admin dashboards share it.
func (s *Store) dailySeries(ctx context.Context, predicate string, args ...any) ([]DailyPoint, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT to_char(d.day, 'YYYY-MM-DD'), count(c.id), COALESCE(sum(c.gross_amount), 0)
   FROM generate_series(date_trunc('day', now() AT TIME ZONE 'Asia/Kuala_Lumpur') - interval '6 days',
                        date_trunc('day', now() AT TIME ZONE 'Asia/Kuala_Lumpur'), interval '1 day') AS d(day)
   LEFT JOIN cdrs c ON c.created_at >= (d.day AT TIME ZONE 'Asia/Kuala_Lumpur')
                  AND c.created_at < ((d.day + interval '1 day') AT TIME ZONE 'Asia/Kuala_Lumpur') AND `+predicate+`
   GROUP BY d.day ORDER BY d.day`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only result set

	out := []DailyPoint{}
	for rows.Next() {
		var p DailyPoint
		if err := rows.Scan(&p.Day, &p.Sessions, &p.Gross); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// LedgerTotals summarises a CDR range (the totals row under the ledger table).
type LedgerTotals struct {
	Currency  string  `json:"currency"`
	CDRs      int     `json:"count"`
	EnergyKWh float64 `json:"energyKwh"`
	Gross     float64 `json:"gross"`
	Fee       float64 `json:"platformFee"`
	Net       float64 `json:"netPayout"`
}

// LedgerRow is one CDR line in the revenue table and the CSV export. It carries
// the station, EVSE and tariff of the session the CDR settled, which a finance
// view needs and the CDR document alone does not name.
type LedgerRow struct {
	CDRID        string    `json:"id"`
	SessionID    string    `json:"sessionId"`
	StartTime    time.Time `json:"startTime"`
	EndTime      time.Time `json:"endTime"`
	StationName  string    `json:"stationName"`
	EVSEUID      string    `json:"evseUid"`
	EnergyKWh    float64   `json:"energyKwh"`
	Minutes      int       `json:"minutes"`
	TariffPerKWh float64   `json:"tariffPerKwh"`
	Gross        float64   `json:"gross"`
	Fee          float64   `json:"platformFee"`
	Net          float64   `json:"netPayout"`
	Currency     string    `json:"currency"`
}

// CDRLedger returns one page of a CPO's CDRs in [from, to) plus the totals over
// the whole range. The totals are deliberately not page-limited, so the summary
// cards stay correct while the table pages. The sandbox driver is excluded. A
// non-empty evseUID narrows both the page and the totals to one bay, which is
// what the network drawer's "recent sessions on this EVSE" reads.
func (s *Store) CDRLedger(ctx context.Context, cpoID, evseUID string, from, to time.Time, limit, offset int, currencies ...string) ([]LedgerRow, LedgerTotals, error) {
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, LedgerTotals{}, err
	}
	defer tx.Rollback() //nolint:errcheck // safe after commit
	out := []LedgerRow{}
	totals, err := readCDRLedger(ctx, tx, cpoID, evseUID, from, to, limit, offset, reportCurrency(currencies), func(row LedgerRow) error {
		out = append(out, row)
		return nil
	})
	if err != nil {
		return nil, LedgerTotals{}, err
	}
	return out, totals, tx.Commit()
}

type ledgerReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// ExportCDRLedger visits all matching rows with bounded application memory.
// Rows and totals share a read-only repeatable snapshot, including concurrent settlements.
func (s *Store) ExportCDRLedger(ctx context.Context, cpoID, evseUID string, from, to time.Time, visit func(LedgerRow) error, currencies ...string) (LedgerTotals, error) {
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return LedgerTotals{}, err
	}
	defer tx.Rollback() //nolint:errcheck // safe after commit
	totals, err := readCDRLedger(ctx, tx, cpoID, evseUID, from, to, nil, 0, reportCurrency(currencies), visit)
	if err != nil {
		return LedgerTotals{}, err
	}
	return totals, tx.Commit()
}

func readCDRLedger(ctx context.Context, db ledgerReader, cpoID, evseUID string, from, to time.Time, limit any, offset int, currency string, visit func(LedgerRow) error) (LedgerTotals, error) {
	totals := LedgerTotals{Currency: currency}
	// $5 = '' means "no EVSE filter"; this keeps the placeholder numbering fixed
	// so the page query can reuse it below.
	evseUID = strings.TrimSpace(evseUID)
	if err := db.QueryRowContext(ctx,
		`SELECT count(*), COALESCE(sum(c.total_energy_kwh), 0), COALESCE(sum(c.gross_amount), 0),
		        COALESCE(sum(c.platform_fee), 0), COALESCE(sum(c.net_payout), 0)
		   FROM cdrs c
		   JOIN charging_sessions cs ON cs.id = c.session_id
		  WHERE c.currency='`+currency+`' AND c.cpo_id = $1 AND c.user_id <> $2 AND c.created_at >= $3 AND c.created_at < $4
		    AND ($5 = '' OR cs.evse_uid = $5)`,
		cpoID, SandboxUserID, from, to, evseUID).
		Scan(&totals.CDRs, &totals.EnergyKWh, &totals.Gross, &totals.Fee, &totals.Net); err != nil {
		return LedgerTotals{}, err
	}

	rows, err := db.QueryContext(ctx,
		`SELECT c.id, c.session_id, cs.start_time, cs.end_time, st.name, cs.evse_uid,
		        c.total_energy_kwh, c.total_time_seconds, cs.tariff_per_kwh,
		        c.gross_amount, c.platform_fee, c.net_payout, c.currency
		   FROM cdrs c
		   JOIN charging_sessions cs ON cs.id = c.session_id
		   JOIN stations st ON st.id = cs.station_id
		  WHERE c.currency='`+currency+`' AND c.cpo_id = $1 AND c.user_id <> $2 AND c.created_at >= $3 AND c.created_at < $4
		    AND ($5 = '' OR cs.evse_uid = $5)
		  ORDER BY c.created_at DESC, c.id DESC
		  LIMIT $6 OFFSET $7`,
		cpoID, SandboxUserID, from, to, evseUID, limit, offset)
	if err != nil {
		return LedgerTotals{}, err
	}
	defer rows.Close() //nolint:errcheck // read-only result set

	for rows.Next() {
		var (
			r       LedgerRow
			end     sql.NullTime
			seconds int
		)
		if err := rows.Scan(&r.CDRID, &r.SessionID, &r.StartTime, &end, &r.StationName, &r.EVSEUID,
			&r.EnergyKWh, &seconds, &r.TariffPerKWh, &r.Gross, &r.Fee, &r.Net, &r.Currency); err != nil {
			return LedgerTotals{}, err
		}
		// A CPO that never reported an end uses the elapsed time it did send.
		r.EndTime = r.StartTime.Add(time.Duration(seconds) * time.Second)
		if end.Valid {
			r.EndTime = end.Time
		}
		r.Minutes = seconds / 60
		if err := visit(r); err != nil {
			return LedgerTotals{}, err
		}
	}
	return totals, rows.Err()
}

// AdminCPORow is one CPO in the super-admin table.
type AdminCPORow struct {
	Currency        string  `json:"currency"`
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	PartyID         string  `json:"partyId"`
	Status          string  `json:"status"`
	HandshakeStatus string  `json:"handshakeStatus"`
	Stations        int     `json:"stations"`
	EVSEs           int     `json:"evses"`
	SessionsToday   int     `json:"sessionsToday"`
	GrossToday      float64 `json:"grossToday"`
	PlatformFeePct  float64 `json:"platformFeePct"`
}

// ListAllCPOsAdmin returns every partner, any status, with its network size and
// today's numbers. The sandbox driver is excluded from the money column.
func (s *Store) ListAllCPOsAdmin(ctx context.Context, currencies ...string) ([]AdminCPORow, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT p.id, p.name, p.party_id, p.status, p.handshake_status, p.platform_fee_pct,
		        (SELECT count(*) FROM stations st WHERE st.cpo_id = p.id),
		        (SELECT count(*) FROM connectors k JOIN stations st ON st.id = k.station_id
		          WHERE st.cpo_id = p.id),
		        (SELECT count(*) FROM charging_sessions cs
		          WHERE cs.cpo_id = p.id AND cs.user_id <> $1
		            AND cs.start_time >= (date_trunc('day', now() AT TIME ZONE 'Asia/Kuala_Lumpur') AT TIME ZONE 'Asia/Kuala_Lumpur') AND cs.start_time < ((date_trunc('day', now() AT TIME ZONE 'Asia/Kuala_Lumpur') + interval '1 day') AT TIME ZONE 'Asia/Kuala_Lumpur')),
		        (SELECT COALESCE(sum(c.gross_amount), 0) FROM cdrs c
		          WHERE c.currency='`+reportCurrency(currencies)+`' AND c.cpo_id = p.id AND c.user_id <> $1
		            AND c.created_at >= (date_trunc('day', now() AT TIME ZONE 'Asia/Kuala_Lumpur') AT TIME ZONE 'Asia/Kuala_Lumpur') AND c.created_at < ((date_trunc('day', now() AT TIME ZONE 'Asia/Kuala_Lumpur') + interval '1 day') AT TIME ZONE 'Asia/Kuala_Lumpur'))
		   FROM cpo_partners p ORDER BY p.name`, SandboxUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only result set

	out := []AdminCPORow{}
	for rows.Next() {
		r := AdminCPORow{Currency: reportCurrency(currencies)}
		if err := rows.Scan(&r.ID, &r.Name, &r.PartyID, &r.Status, &r.HandshakeStatus,
			&r.PlatformFeePct, &r.Stations, &r.EVSEs, &r.SessionsToday, &r.GrossToday); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetCPOStatus changes a partner's status and, when feePct is not nil, its
// platform fee — administrator approval/suspension and fee editor.
func (s *Store) SetCPOStatus(ctx context.Context, cpoID, status string, feePct *float64) error {
	res, err := s.DB.ExecContext(ctx,
		`UPDATE cpo_partners
		    SET status = $2, platform_fee_pct = COALESCE($3, platform_fee_pct)
		  WHERE id = $1`, cpoID, status, feePct)
	if err != nil {
		return err
	}
	return requireOneRow(res)
}

// AdminOverview is `GET /admin/api/admin/overview`: network-wide KPIs and the
// shared 7-day chart.
type AdminOverview struct {
	Currency      string       `json:"currency"`
	CPOs          int          `json:"cpos"`
	ActiveCPOs    int          `json:"activeCpos"`
	Stations      int          `json:"stations"`
	EVSEs         int          `json:"evses"`
	SessionsToday int          `json:"sessionsToday"`
	GrossToday    float64      `json:"grossToday"`
	Last7Days     []DailyPoint `json:"last7Days"`
}

// AdminOverview computes the super-admin dashboard across every partner.
func (s *Store) AdminOverview(ctx context.Context, currencies ...string) (*AdminOverview, error) {
	out := &AdminOverview{Last7Days: []DailyPoint{}, Currency: reportCurrency(currencies)}
	if err := s.DB.QueryRowContext(ctx,
		`SELECT (SELECT count(*) FROM cpo_partners),
		        (SELECT count(*) FROM cpo_partners WHERE status = 'ACTIVE'),
		        (SELECT count(*) FROM stations),
		        (SELECT count(*) FROM connectors),
		        (SELECT count(*) FROM charging_sessions
		          WHERE user_id <> $1 AND start_time >= (date_trunc('day', now() AT TIME ZONE 'Asia/Kuala_Lumpur') AT TIME ZONE 'Asia/Kuala_Lumpur') AND start_time < ((date_trunc('day', now() AT TIME ZONE 'Asia/Kuala_Lumpur') + interval '1 day') AT TIME ZONE 'Asia/Kuala_Lumpur')),
		        (SELECT COALESCE(sum(gross_amount), 0) FROM cdrs
		          WHERE currency='`+out.Currency+`' AND user_id <> $1 AND created_at >= (date_trunc('day', now() AT TIME ZONE 'Asia/Kuala_Lumpur') AT TIME ZONE 'Asia/Kuala_Lumpur') AND created_at < ((date_trunc('day', now() AT TIME ZONE 'Asia/Kuala_Lumpur') + interval '1 day') AT TIME ZONE 'Asia/Kuala_Lumpur'))`,
		SandboxUserID).
		Scan(&out.CPOs, &out.ActiveCPOs, &out.Stations, &out.EVSEs, &out.SessionsToday, &out.GrossToday); err != nil {
		return nil, err
	}

	daily, err := s.dailySeries(ctx, `c.user_id <> $1 AND c.currency='`+out.Currency+`'`, SandboxUserID)
	if err != nil {
		return nil, err
	}
	out.Last7Days = daily
	return out, nil
}

// PortalCDR is one CDR opened from the revenue ledger: the table row plus the
// stored OCPI document (cdrs.cdr_json), which the console pretty-prints in its
// detail modal.
type PortalCDR struct {
	Row      LedgerRow      `json:"row"`
	Document map[string]any `json:"document"`
}

// CDRDetail returns ErrNotFound for foreign and sandbox CDRs so a CPO cannot
// discover that those identifiers exist.
func (s *Store) CDRDetail(ctx context.Context, cpoID, cdrID string) (*PortalCDR, error) {
	out := &PortalCDR{}
	var (
		end     sql.NullTime
		seconds int
		doc     []byte
	)
	err := s.DB.QueryRowContext(ctx,
		`SELECT c.id, c.session_id, cs.start_time, cs.end_time, st.name, cs.evse_uid,
		        c.total_energy_kwh, c.total_time_seconds, cs.tariff_per_kwh,
		        c.gross_amount, c.platform_fee, c.net_payout, c.currency, c.cdr_json
		   FROM cdrs c
		   JOIN charging_sessions cs ON cs.id = c.session_id
		   JOIN stations st ON st.id = cs.station_id
		  WHERE c.id = $1 AND c.cpo_id = $2 AND c.user_id <> $3`,
		cdrID, cpoID, SandboxUserID).
		Scan(&out.Row.CDRID, &out.Row.SessionID, &out.Row.StartTime, &end, &out.Row.StationName,
			&out.Row.EVSEUID, &out.Row.EnergyKWh, &seconds, &out.Row.TariffPerKWh,
			&out.Row.Gross, &out.Row.Fee, &out.Row.Net, &out.Row.Currency, &doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	out.Row.EndTime = out.Row.StartTime.Add(time.Duration(seconds) * time.Second)
	if end.Valid {
		out.Row.EndTime = end.Time
	}
	out.Row.Minutes = seconds / 60

	out.Document = map[string]any{}
	if len(doc) > 0 {
		if err := json.Unmarshal(doc, &out.Document); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Currency identifiers interpolated into report SQL are selected only from this
// closed set. No user-supplied SQL or unlabelled mixed-currency totals.
func reportCurrency(values []string) string {
	if len(values) > 0 && ValidCurrency(values[0]) {
		return values[0]
	}
	return CurrencyMYR
}
