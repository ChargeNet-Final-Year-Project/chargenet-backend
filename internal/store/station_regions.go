package store

import "context"

// StationRegion describes actual active coverage, not a fabricated list of
// states with no chargers. Bounds let explicit region selection frame its sites.
type StationRegion struct {
	CountryCode  string  `json:"countryCode"`
	Name         string  `json:"name"`
	StationCount int     `json:"stationCount"`
	South        float64 `json:"south"`
	West         float64 `json:"west"`
	North        float64 `json:"north"`
	East         float64 `json:"east"`
}

func (s *Store) ListStationRegions(ctx context.Context) ([]StationRegion, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT s.country_code,s.region,count(*),
 min(s.latitude),min(s.longitude),max(s.latitude),max(s.longitude)
 FROM stations s JOIN cpo_partners c ON c.id=s.cpo_id
 WHERE s.is_active AND c.status='ACTIVE' AND s.region<>''
 GROUP BY s.country_code,s.region ORDER BY s.country_code,s.region`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StationRegion{}
	for rows.Next() {
		var region StationRegion
		if err := rows.Scan(&region.CountryCode, &region.Name, &region.StationCount, &region.South, &region.West, &region.North, &region.East); err != nil {
			return nil, err
		}
		out = append(out, region)
	}
	return out, rows.Err()
}
