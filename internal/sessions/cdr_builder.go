package sessions

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/chargenet/chargenet-backend/internal/store"
)

// BuildCDR renders the OCPI-shaped CDR that settlement stores in
// `cdrs.cdr_json`.
//
// It is a pure function: [store.SettleSession] calls it inside the settlement
// transaction, with the amounts that transaction is about to commit, so the
// document can never disagree with the ledger. The document carries the OCPI
// subset the portal displays plus ChargeNet's non-OCPI `chargenet` block with
// the platform fee split.
func BuildCDR(req store.CDRRequest) ([]byte, error) {
	a := req.Amounts
	start := a.StartTime.UTC().Format(time.RFC3339)
	end := a.EndTime.UTC().Format(time.RFC3339)
	hours := round4(float64(a.DurationSeconds) / 3600)

	locationID := req.Station.OCPILocationID
	if locationID == "" {
		locationID = req.Station.ID
	}
	connectorID := req.Connector.OCPIConnectorID
	if connectorID == "" {
		connectorID = req.Connector.ID
	}
	evseUID, _, _ := strings.Cut(req.Connector.EVSEUID, ":")
	doc := map[string]any{
		"country_code":    req.CPOCountryCode,
		"party_id":        req.CPOPartyID,
		"id":              a.CDRID,
		"start_date_time": start,
		"end_date_time":   end,
		"session_id":      req.Session.ID,
		"cdr_token": map[string]any{
			"uid":          req.Session.UserID,
			"type":         "APP_USER",
			"contract_id":  req.Session.UserID,
			"country_code": "MY", "party_id": "CNT",
		},
		"auth_method": "COMMAND",
		"cdr_location": map[string]any{
			"id":                   locationID,
			"name":                 req.Station.Name,
			"address":              req.Station.Address,
			"city":                 req.Station.City,
			"country":              alpha3(req.Station.CountryCode),
			"coordinates":          coordinates(req.Station.Latitude, req.Station.Longitude),
			"evse_uid":             evseUID,
			"connector_id":         connectorID,
			"connector_standard":   wireStandard(req.Connector.Standard),
			"connector_format":     req.Connector.Format,
			"connector_power_type": wirePower(req.Connector.PowerType),
			"evse_id":              evseUID,
		},
		"currency": a.Currency,
		"tariffs": []any{map[string]any{
			"id":           req.TariffID,
			"country_code": req.CPOCountryCode, "party_id": req.CPOPartyID, "last_updated": start,
			"currency": a.Currency,
			"elements": []any{map[string]any{
				"price_components": []any{map[string]any{
					"type":      "ENERGY",
					"price":     a.TariffPerKWh,
					"step_size": 1,
				}},
			}},
		}},
		"charging_periods": []any{map[string]any{
			"start_date_time": start,
			"dimensions": []any{
				map[string]any{"type": "ENERGY", "volume": round3(a.EnergyKWh)},
				map[string]any{"type": "TIME", "volume": hours},
			},
		}},
		"total_cost":   map[string]any{"excl_vat": a.Gross},
		"total_energy": round3(a.EnergyKWh),
		"total_time":   hours,
		"last_updated": end,
	}

	if a.OCPIVersion == "2.3.0" {
		doc["total_cost"] = map[string]any{"before_taxes": a.Gross}
		for _, item := range doc["tariffs"].([]any) {
			item.(map[string]any)["tax_included"] = "N/A"
		}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("sessions: encode cdr: %w", err)
	}
	return raw, nil
}

// coordinates renders OCPI coordinates as the decimal strings required by the spec.
func coordinates(lat, lng float64) map[string]any {
	return map[string]any{
		"latitude":  fmt.Sprintf("%.6f", lat),
		"longitude": fmt.Sprintf("%.6f", lng),
	}
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

func round4(v float64) float64 { return math.Round(v*10000) / 10000 }

func alpha3(v string) string {
	switch v {
	case "SG":
		return "SGP"
	case "TH":
		return "THA"
	}
	return "MYS"
}
func wirePower(v string) string {
	if v == "AC" {
		return "AC_3_PHASE"
	}
	return "DC"
}
func wireStandard(v string) string {
	switch v {
	case "CCS2":
		return "IEC_62196_T2_COMBO"
	case "TYPE2":
		return "IEC_62196_T2"
	case "CCS1":
		return "IEC_62196_T1_COMBO"
	case "NACS":
		return "NACS"
	case "GBT":
		return "GBT_DC"
	}
	return v
}
