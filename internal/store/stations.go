package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/chargenet/chargenet-backend/internal/geo"
)

// ListCpos returns every ACTIVE CPO partner.
func (s *Store) ListCpos(ctx context.Context) ([]CpoPartner, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, name, country_code, party_id, color_hex
		   FROM cpo_partners WHERE status = 'ACTIVE' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []CpoPartner{}
	for rows.Next() {
		var c CpoPartner
		if err := rows.Scan(&c.ID, &c.Name, &c.CountryCode, &c.PartyID, &c.ColorHex); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListStations returns active stations of ACTIVE CPOs matching filter, with
// all of their connectors populated. Distance is filled and drives the sort
// order when the filter carries coordinates.
func (s *Store) ListStations(ctx context.Context, filter StationFilter) ([]Station, error) {
	var args []any
	conds := []string{"s.is_active", "c.status = 'ACTIVE'"}

	if filter.CountryCode != "" {
		args = append(args, filter.CountryCode)
		conds = append(conds, fmt.Sprintf("s.country_code=$%d", len(args)))
	}
	if filter.Region != "" {
		args = append(args, filter.Region)
		conds = append(conds, fmt.Sprintf("s.region=$%d", len(args)))
	}
	if filter.CpoID != "" {
		args = append(args, filter.CpoID)
		conds = append(conds, fmt.Sprintf("s.cpo_id = $%d", len(args)))
	}
	if q := strings.TrimSpace(filter.Query); q != "" {
		args = append(args, "%"+q+"%")
		p := fmt.Sprintf("$%d", len(args))
		conds = append(conds, fmt.Sprintf("(s.name ILIKE %s OR s.address ILIKE %s OR s.city ILIKE %s)", p, p, p))
	}

	// Connector-level predicates expressed as EXISTS so that ALL connectors of
	// a matching station are still returned.
	var connConds []string
	if filter.PowerType != "" {
		args = append(args, filter.PowerType)
		connConds = append(connConds, fmt.Sprintf("k.power_type=$%d", len(args)))
	}
	if filter.MaxPowerKW > 0 {
		args = append(args, filter.MaxPowerKW)
		connConds = append(connConds, fmt.Sprintf("k.max_power_kw <= $%d", len(args)))
	}
	if filter.ConnectorType != "" {
		args = append(args, strings.ToUpper(strings.TrimSpace(filter.ConnectorType)))
		connConds = append(connConds, fmt.Sprintf("k.standard = $%d", len(args)))
	}
	if filter.AvailableOnly {
		connConds = append(connConds, "k.status = 'AVAILABLE'")
	}
	if filter.MinPowerKW > 0 {
		args = append(args, filter.MinPowerKW)
		connConds = append(connConds, fmt.Sprintf("k.max_power_kw >= $%d", len(args)))
	}
	if len(connConds) > 0 {
		conds = append(conds, "EXISTS (SELECT 1 FROM connectors k WHERE k.station_id = s.id AND "+strings.Join(connConds, " AND ")+")")
	}

	query := `SELECT s.id, s.cpo_id, c.name, c.color_hex, s.name, s.address, s.city,
	                 s.latitude, s.longitude, s.total_bays, s.available_bays, s.is_active,s.country_code,s.is_simulated,s.region
	            FROM stations s
	            JOIN cpo_partners c ON c.id = s.cpo_id
	           WHERE ` + strings.Join(conds, " AND ")

	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	stations := []Station{}
	for rows.Next() {
		var st Station
		if err := rows.Scan(&st.ID, &st.CpoID, &st.CpoName, &st.CpoColorHex, &st.Name,
			&st.Address, &st.City, &st.Latitude, &st.Longitude, &st.TotalBays,
			&st.AvailableBays, &st.IsActive, &st.CountryCode, &st.IsSimulated, &st.Region); err != nil {
			return nil, err
		}
		stations = append(stations, st)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(stations) == 0 {
		return stations, nil
	}

	ids := make([]string, 0, len(stations))
	for _, st := range stations {
		ids = append(ids, st.ID)
	}
	connectors, err := s.connectorsByStation(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range stations {
		if c := connectors[stations[i].ID]; c != nil {
			stations[i].Connectors = c
		} else {
			stations[i].Connectors = []Connector{}
		}
	}

	if filter.Lat != nil && filter.Lng != nil {
		kept := stations[:0]
		for _, st := range stations {
			d := geo.Round1(geo.HaversineKM(*filter.Lat, *filter.Lng, st.Latitude, st.Longitude))
			if filter.RadiusKm != nil && d > *filter.RadiusKm {
				continue
			}
			st.DistanceKm = &d
			kept = append(kept, st)
		}
		stations = kept
		sort.SliceStable(stations, func(i, j int) bool {
			return *stations[i].DistanceKm < *stations[j].DistanceKm
		})
	} else {
		sort.SliceStable(stations, func(i, j int) bool { return stations[i].Name < stations[j].Name })
	}

	return stations, nil
}

// GetStation returns a single station with connectors, or ErrNotFound.
//
// A station whose partner is not ACTIVE is reported as [ErrCpoInactive], the
// same as in [Store.GetEVSE]: suspending a CPO hides its stations
// from discovery, and a link that predates the suspension must not resurrect
// one.
func (s *Store) GetStation(ctx context.Context, id string) (*Station, error) {
	var (
		st       Station
		cpoState string
	)
	err := s.DB.QueryRowContext(ctx,
		`SELECT s.id, s.cpo_id, c.name, c.color_hex, s.name, s.address, s.city,
		        s.latitude, s.longitude, s.total_bays, s.available_bays, s.is_active,s.country_code,s.is_simulated,s.region,
		        c.status
		   FROM stations s JOIN cpo_partners c ON c.id = s.cpo_id
		  WHERE s.id = $1`, id).
		Scan(&st.ID, &st.CpoID, &st.CpoName, &st.CpoColorHex, &st.Name, &st.Address,
			&st.City, &st.Latitude, &st.Longitude, &st.TotalBays, &st.AvailableBays, &st.IsActive, &st.CountryCode, &st.IsSimulated, &st.Region,
			&cpoState)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if cpoState != CPOStatusActive {
		return nil, ErrCpoInactive
	}
	conns, err := s.connectorsByStation(ctx, []string{st.ID})
	if err != nil {
		return nil, err
	}
	if c := conns[st.ID]; c != nil {
		st.Connectors = c
	} else {
		st.Connectors = []Connector{}
	}
	return &st, nil
}

// GetEVSE resolves a scanned EVSE uid to its station and connector.
//
// A connector whose partner is not ACTIVE is reported as [ErrCpoInactive].
// This keeps suspended stations unavailable through scanned codes.
func (s *Store) GetEVSE(ctx context.Context, evseUID string) (*Station, *Connector, error) {
	var connID, stationID, cpoStatus string
	err := s.DB.QueryRowContext(ctx,
		`SELECT k.id, k.station_id, c.status
		   FROM connectors k
		   JOIN stations s ON s.id = k.station_id
		   JOIN cpo_partners c ON c.id = s.cpo_id
		  WHERE k.evse_uid = $1`,
		strings.ToUpper(strings.TrimSpace(evseUID))).Scan(&connID, &stationID, &cpoStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	if cpoStatus != CPOStatusActive {
		return nil, nil, ErrCpoInactive
	}

	st, err := s.GetStation(ctx, stationID)
	if err != nil {
		return nil, nil, err
	}
	for i := range st.Connectors {
		if st.Connectors[i].ID == connID {
			return st, &st.Connectors[i], nil
		}
	}
	return nil, nil, ErrNotFound
}

// SetConnectorStatus updates a connector and recomputes its parent station's
// available_bays in one transaction.
func (s *Store) SetConnectorStatus(ctx context.Context, connectorID, status string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	if err := setConnectorStatusTx(ctx, tx, connectorID, status); err != nil {
		return err
	}
	return tx.Commit()
}

// setConnectorStatusTx sets a connector's status and recomputes its station's
// available_bays inside the caller's transaction. Sessions use it so that
// "connector goes CHARGING/AVAILABLE" and the session row change commit
// together.
func setConnectorStatusTx(ctx context.Context, tx *sql.Tx, connectorID, status string) error {
	var stationID string
	err := tx.QueryRowContext(ctx,
		`UPDATE connectors SET status = $1, last_updated = NOW() WHERE id = $2 RETURNING station_id`,
		status, connectorID).Scan(&stationID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx,
		`UPDATE stations
		    SET available_bays = (SELECT count(*) FROM connectors WHERE station_id = $1 AND status = 'AVAILABLE')
		  WHERE id = $1`, stationID)
	return err
}

func (s *Store) connectorsByStation(ctx context.Context, stationIDs []string) (map[string][]Connector, error) {
	out := map[string][]Connector{}
	if len(stationIDs) == 0 {
		return out, nil
	}

	args := make([]any, len(stationIDs))
	parts := make([]string, len(stationIDs))
	for i, id := range stationIDs {
		args[i] = id
		parts[i] = fmt.Sprintf("$%d", i+1)
	}
	query := `SELECT k.id,k.station_id,k.evse_uid,k.standard,k.format,k.power_type,k.max_power_kw,coalesce(t.price_per_kwh,k.tariff_per_kwh),k.status,coalesce(t.currency,'MYR'),k.tariff_id
 FROM connectors k LEFT JOIN tariffs t ON t.id=k.tariff_id WHERE station_id IN (` + strings.Join(parts, ", ") + `)
	           ORDER BY evse_uid`

	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var c Connector
		if err := rows.Scan(&c.ID, &c.StationID, &c.EVSEUID, &c.Standard, &c.Format,
			&c.PowerType, &c.MaxPowerKW, &c.TariffPerKWh, &c.Status, &c.Currency, &c.TariffID); err != nil {
			return nil, err
		}
		out[c.StationID] = append(out[c.StationID], c)
	}
	return out, rows.Err()
}
