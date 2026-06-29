package store

import "strings"

// Canonical connector standards.
const (
	StandardCCS2    = "CCS2"
	StandardTYPE2   = "TYPE2"
	StandardCHADEMO = "CHADEMO"
	StandardNACS    = "NACS"
	StandardCCS1    = "CCS1"
	StandardGBT     = "GBT"
)

// Connector and session status values.
const (
	StatusAvailable   = "AVAILABLE"
	StatusCharging    = "CHARGING"
	StatusBlocked     = "BLOCKED"
	StatusInoperative = "INOPERATIVE"
	StatusPending     = "PENDING"
	StatusCompleted   = "COMPLETED"
	StatusFailed      = "FAILED"
)

// ConnectorStandards lists every accepted value for vehicles.connector_type.
var ConnectorStandards = []string{StandardCCS2, StandardTYPE2, StandardCHADEMO, StandardNACS, StandardCCS1, StandardGBT}

// ValidConnectorStandard reports whether s is a canonical connector standard.
// The comparison is case-insensitive.
func ValidConnectorStandard(s string) bool {
	for _, v := range ConnectorStandards {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// Accepted battery capacity range in kWh for a vehicle.
const (
	MinBatteryCapacity = 10.0
	MaxBatteryCapacity = 250.0
)

// CpoPartner is the {id,name,countryCode,partyId,colorHex} DTO.
type CpoPartner struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	CountryCode string `json:"countryCode"`
	PartyID     string `json:"partyId"`
	ColorHex    string `json:"colorHex"`
}

// Connector is the public connector DTO.
type Connector struct {
	OCPIConnectorID string  `json:"-"`
	Currency        string  `json:"currency"`
	TariffID        *string `json:"tariffId"`
	ID              string  `json:"id"`
	StationID       string  `json:"stationId"`
	EVSEUID         string  `json:"evseUid"`
	Standard        string  `json:"standard"`
	Format          string  `json:"format"`
	PowerType       string  `json:"powerType"`
	MaxPowerKW      float64 `json:"maxPowerKw"`
	TariffPerKWh    float64 `json:"tariffPerKwh"`
	Status          string  `json:"status"`
}

// Station is the public station DTO. DistanceKm is only set when the caller
// supplied lat/lng.
type Station struct {
	Region        string      `json:"region"`
	CountryCode   string      `json:"countryCode"`
	IsSimulated   bool        `json:"isSimulated"`
	ID            string      `json:"id"`
	CpoID         string      `json:"cpoId"`
	CpoName       string      `json:"cpoName"`
	CpoColorHex   string      `json:"cpoColorHex"`
	Name          string      `json:"name"`
	Address       string      `json:"address"`
	City          string      `json:"city"`
	Latitude      float64     `json:"latitude"`
	Longitude     float64     `json:"longitude"`
	TotalBays     int         `json:"totalBays"`
	AvailableBays int         `json:"availableBays"`
	IsActive      bool        `json:"isActive"`
	DistanceKm    *float64    `json:"distanceKm"`
	Connectors    []Connector `json:"connectors"`
}

// StationFilter mirrors the GET /stations query parameters.
type StationFilter struct {
	Region        string
	PowerType     string
	MaxPowerKW    float64
	CountryCode   string
	Query         string
	ConnectorType string
	CpoID         string
	AvailableOnly bool
	MinPowerKW    float64
	Lat           *float64
	Lng           *float64
	RadiusKm      *float64
}
