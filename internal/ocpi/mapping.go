package ocpi

import "strings"

// maxElectricPowerW is the OCPI way of stating a connector's power: watts. The
// canonical model stores kW, so every push divides by a thousand.
const wattsPerKW = 1000

// canonicalStandard rejects standards the mobile application cannot represent.
//
// `TESLA_S`/`TESLA_X` are the spec's own names for ChargeNet's legacy `TESLA` value;
// all three are the NACS plug ChargeNet models. IEC_62196_T1 (a plain Type 1)
// and IEC_62196_T3 have no canonical equivalent here.
func canonicalStandard(ocpi string) (string, bool) {
	switch strings.ToUpper(strings.TrimSpace(ocpi)) {
	case "IEC_62196_T2":
		return "TYPE2", true
	case "IEC_62196_T2_COMBO":
		return "CCS2", true
	case "CHADEMO":
		return "CHADEMO", true
	case "IEC_62196_T1_COMBO":
		return "CCS1", true
	case "GBT_DC", "GBT_AC":
		return "GBT", true
	case "NACS", "TESLA", "TESLA_S", "TESLA_X":
		return "NACS", true
	default:
		return "", false
	}
}

// canonicalStatus maps an OCPI EVSE status onto the canonical subset
// The statuses the driver app cannot represent collapse
// onto the nearest one it can: a reserved or blocked bay is BLOCKED, everything
// a CPO calls broken or gone is INOPERATIVE.
func canonicalStatus(ocpi string) (string, bool) {
	switch strings.ToUpper(strings.TrimSpace(ocpi)) {
	case EVSEAvailable:
		return "AVAILABLE", true
	case EVSECharging:
		return "CHARGING", true
	case EVSEBlocked, EVSEReserved:
		return "BLOCKED", true
	case EVSEInoperative, EVSEOutOfOrder, EVSEUnknown, EVSERemoved:
		return "INOPERATIVE", true
	default:
		return "", false
	}
}

// connectorPowerKW accepts OCPI's direct watt value or voltage and amperage.
// Missing power becomes 0 kW so the mobile can show the connector without
// promising a speed.
func connectorPowerKW(c Connector) float64 {
	if c.MaxElectricPower > 0 {
		return float64(c.MaxElectricPower) / wattsPerKW
	}
	if c.MaxVoltage > 0 && c.MaxAmperage > 0 {
		return float64(c.MaxVoltage) * c.MaxAmperage / wattsPerKW
	}
	return 0
}

// connectorFormat and connectorPowerType pass the spec's own values through,
// uppercased: the canonical model keeps SOCKET/CABLE and AC/DC verbatim because
// the mobile renders them. An empty value takes the safe default.
func connectorFormat(ocpi string) string {
	switch strings.ToUpper(strings.TrimSpace(ocpi)) {
	case "SOCKET":
		return "SOCKET"
	default:
		return "CABLE"
	}
}

func connectorPowerType(ocpi string) string {
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(ocpi)), "AC") {
		return "AC"
	}
	return "DC"
}

// EVSEUIDFor names the ChargeNet EVSE uid one OCPI connector becomes.
//
// OCPI nests connectors inside an EVSE and ChargeNet keys a row by the EVSE uid
// alone (the driver scans one code per plug), so the first connector of an EVSE
// keeps the uid and any further one is suffixed with its OCPI connector id. Two
// sockets on one charge point therefore stay two scannable rows, which is how
// ChargeNet counts bays.
func EVSEUIDFor(evseUID, connectorID string, index int) string {
	if index == 0 {
		return evseUID
	}
	return evseUID + evseUIDSeparator + connectorID
}

// evseUIDSeparator joins an EVSE uid to the connector id of a second socket.
const evseUIDSeparator = ":"

// CPOEVSEUID strips the suffix [EVSEUIDFor] adds, so an outbound command names
// the EVSE the way its CPO does. The store applies the same rule when a PATCH
// arrives for an EVSE with more than one connector.
func CPOEVSEUID(evseUID string) string {
	base, _, _ := strings.Cut(evseUID, evseUIDSeparator)
	return base
}

// firstEnergyPrice picks the price of the first ENERGY component of a tariff
// . ChargeNet's tariff model is per-kWh, so a TIME
// component — a parking fee, say — is deliberately not folded into it.
func firstEnergyPrice(t Tariff) float64 {
	for _, el := range t.Elements {
		for _, pc := range el.PriceComponents {
			if strings.EqualFold(pc.Type, "ENERGY") {
				return pc.Price
			}
		}
	}
	return 0
}
