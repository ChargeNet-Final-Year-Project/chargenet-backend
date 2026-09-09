package emulator

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chargenet/chargenet-backend/internal/ocpi"
	"github.com/chargenet/chargenet-backend/internal/store"
)

// wattsPerKW is the OCPI way of stating a connector's power (watts). ChargeNet
// stores kW, so a rendered location multiplies back (the inverse of the
// receiver's division).
const wattsPerKW = 1000

// chaos holds the failure switches used by demos and tests.
type chaos struct {
	// Offline makes every command answer 503, so ChargeNet sees
	// CPO_UNREACHABLE.
	Offline bool `json:"offline"`
	// RejectNext makes the next START_SESSION answer REJECTED once.
	RejectNext bool `json:"reject_next"`
	// FaultAfterSeconds fails a freshly started session after that many real
	// seconds (0 = off).
	FaultAfterSeconds int `json:"fault_after_seconds"`
}

// SetOffline turns the "CPO is down" scenario on or off. While it is on, every
// command answers 503, so a start through ChargeNet fails with CPO_UNREACHABLE
// before any money moves.
func (e *Emulator) SetOffline(off bool) {
	e.mu.Lock()
	e.chaos.Offline = off
	e.mu.Unlock()
}

// RejectNext makes the next START_SESSION answer REJECTED once — the connector a
// CPO will not start.
func (e *Emulator) RejectNext() {
	e.mu.Lock()
	e.chaos.RejectNext = true
	e.mu.Unlock()
}

// FaultAfterSeconds makes every session the emulator starts fail that many real
// seconds in, so the FAULT path can be exercised. Zero turns it off.
func (e *Emulator) FaultAfterSeconds(seconds int) {
	e.mu.Lock()
	if seconds < 0 {
		seconds = 0
	}
	e.chaos.FaultAfterSeconds = seconds
	e.mu.Unlock()
}

// Scenario reports the current chaos knobs. It is what the admin endpoint
// answers with, so a caller can read them back.
func (e *Emulator) Scenario() chaos {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.chaos
}

// takeReject consumes the "reject the next start" knob, reporting whether it was
// set.
func (e *Emulator) takeReject() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	rejected := e.chaos.RejectNext
	e.chaos.RejectNext = false
	return rejected
}

// scenarioRequest is the body of the admin knob endpoint.
type scenarioRequest struct {
	Offline           *bool `json:"offline"`
	RejectNext        *bool `json:"reject_next"`
	FaultAfterSeconds *int  `json:"fault_after_seconds"`
}

// handleScenario drives the chaos knobs. It is the manual
// (curl) equivalent of the setters, so a demo can put an emulator OFFLINE or
// make it fault without a rebuild.
func (e *Emulator) handleScenario(w http.ResponseWriter, r *http.Request) {
	var req scenarioRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
		ocpi.Fail(w, http.StatusBadRequest, ocpi.StatusClientError, "The scenario could not be parsed.")
		return
	}
	if req.Offline != nil {
		e.SetOffline(*req.Offline)
	}
	if req.RejectNext != nil && *req.RejectNext {
		e.RejectNext()
	}
	if req.FaultAfterSeconds != nil {
		e.FaultAfterSeconds(*req.FaultAfterSeconds)
	}
	ocpi.OK(w, http.StatusOK, e.Scenario())
}

// handleVersions serves GET /versions — the first call a registering CPO makes
// , answering with the one version this emulator speaks.
func (e *Emulator) handleVersions(w http.ResponseWriter, r *http.Request) {
	ocpi.OK(w, http.StatusOK, []ocpi.Version{
		{Version: ocpi.VersionNumber, URL: e.SelfURL() + "/" + ocpi.VersionNumber + "/"},
	})
}

// handleVersionDetails serves GET /2.3.0/ — the endpoint list a registered
// partner reads. A CPO SENDs locations, tariffs and commands.
func (e *Emulator) handleVersionDetails(w http.ResponseWriter, r *http.Request) {
	root := e.SelfURL() + "/" + ocpi.VersionNumber + "/"
	ocpi.OK(w, http.StatusOK, ocpi.VersionDetails{
		Version: ocpi.VersionNumber,
		Endpoints: []ocpi.Endpoint{
			{Identifier: ocpi.ModuleLocations, Role: ocpi.InterfaceSender, URL: root + ocpi.ModuleLocations},
			{Identifier: ocpi.ModuleTariffs, Role: ocpi.InterfaceSender, URL: root + ocpi.ModuleTariffs},
			{Identifier: ocpi.ModuleCommands, Role: ocpi.InterfaceReceiver, URL: root + ocpi.ModuleCommands},
		},
	})
}

// handleLocations serves GET /2.3.0/locations: the CPO's own locations, rendered
// from the rows the platform seeded for it, so the handshake test and OCPI
// gateway pull sync have data to read.
func (e *Emulator) handleLocations(w http.ResponseWriter, r *http.Request) {
	locs, err := e.locationDocs(r.Context())
	if err != nil {
		ocpi.Fail(w, http.StatusInternalServerError, ocpi.StatusUnableToUseAPI, "The locations could not be read.")
		return
	}
	ocpi.OK(w, http.StatusOK, locs)
}

// handleTariffs serves GET /2.3.0/tariffs: the prices the emulator's connectors
// reference.
func (e *Emulator) handleTariffs(w http.ResponseWriter, r *http.Request) {
	ocpi.OK(w, http.StatusOK, e.tariffDocs(r.Context()))
}

// locationDocs renders the CPO's locations the way it pushed them:
// ChargeNet's rows for this partner, mapped back onto OCPI's vocabulary and
// nested EVSE/connector shape. The statuses come from the emulator's own running
// state, so a live session shows its EVSE CHARGING.
func (e *Emulator) locationDocs(ctx context.Context) ([]ocpi.Location, error) {
	if e.store == nil {
		return []ocpi.Location{}, nil
	}
	rows, err := e.store.ListOCPILocations(ctx, e.profile.CPOID)
	if err != nil {
		return nil, err
	}
	out := make([]ocpi.Location, 0, len(rows))
	for _, loc := range rows {
		out = append(out, e.locationDoc(loc))
	}
	return out, nil
}

// locationDoc renders one stored location as an OCPI Location, grouping its
// ChargeNet connector rows back under the EVSEs they came from.
func (e *Emulator) locationDoc(loc store.OCPILocationRead) ocpi.Location {
	type evseGroup struct {
		uid        string
		connectors []store.OCPIConnectorRead
	}
	order := []string{}
	groups := map[string]*evseGroup{}
	for _, c := range loc.Connectors {
		base := ocpi.CPOEVSEUID(c.EVSEUID)
		got, ok := groups[base]
		if !ok {
			got = &evseGroup{uid: base}
			groups[base] = got
			order = append(order, base)
		}
		got.connectors = append(got.connectors, c)
	}

	doc := ocpi.Location{
		Country:     ocpi.CountryAlpha3(loc.CountryCode),
		Publish:     true,
		TimeZone:    ocpi.CountryTimeZone(loc.CountryCode),
		CountryCode: e.profile.CountryCode,
		PartyID:     e.profile.PartyID,
		ID:          loc.LocationID,
		Name:        loc.Name,
		Address:     loc.Address,
		City:        loc.City,
		State:       loc.Region,
		Coordinates: ocpi.GeoLocation{
			Latitude:  strconv.FormatFloat(loc.Latitude, 'f', 6, 64),
			Longitude: strconv.FormatFloat(loc.Longitude, 'f', 6, 64),
		},
		LastUpdated: nowRFC3339(),
	}
	for _, uid := range order {
		group := groups[uid]
		ev := ocpi.EVSE{
			UID:    uid,
			Status: e.evseStatus(uid, group.connectors[0].Status), LastUpdated: nowRFC3339(),
		}
		for i, c := range group.connectors {
			ev.Connectors = append(ev.Connectors, ocpi.Connector{
				ID:               connectorID(c, i),
				Standard:         ocpiStandard(c.Standard),
				Format:           c.Format,
				PowerType:        wirePowerType(c.PowerType),
				MaxElectricPower: int(math.Round(c.MaxPowerKW * wattsPerKW)),
				MaxVoltage:       400, MaxAmperage: c.MaxPowerKW * 1000 / 400, LastUpdated: nowRFC3339(),
				TariffIDs: tariffIDs(c.TariffID),
			})
		}
		doc.EVSEs = append(doc.EVSEs, ev)
	}
	return doc
}

// tariffDocs renders the tariffs the emulator's connectors reference, one per
// distinct tariff id.
// The price is the connector's stored per-kWh price, which is what the platform
// seeded for this partner.
func (e *Emulator) tariffDocs(ctx context.Context) []ocpi.Tariff {
	if e.store == nil {
		return []ocpi.Tariff{}
	}
	rows, err := e.store.ListOCPILocations(ctx, e.profile.CPOID)
	if err != nil {
		return []ocpi.Tariff{}
	}
	seen := map[string]bool{}
	out := []ocpi.Tariff{}
	for _, loc := range rows {
		for _, c := range loc.Connectors {
			id := strings.TrimSpace(c.TariffID)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, ocpi.Tariff{
				CountryCode: e.profile.CountryCode,
				PartyID:     e.profile.PartyID,
				ID:          id,
				Currency:    c.Currency, TaxIncluded: "N/A",
				Elements: []ocpi.TariffElement{{
					PriceComponents: []ocpi.PriceComponent{{Type: "ENERGY", Price: c.TariffPerKWh, StepSize: 1}},
				}},
				LastUpdated: nowRFC3339(),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// setStatus records the canonical status of one OCPI EVSE, so a running session
// shows on the location it is charging at.
func (e *Emulator) setStatus(evseUID, status string) {
	e.mu.Lock()
	e.statuses[evseUID] = status
	e.mu.Unlock()
}

// evseStatus renders an EVSE's current OCPI status, defaulting to what the
// stored rows say for an EVSE the emulator has not touched.
func (e *Emulator) evseStatus(evseUID, fallback string) string {
	e.mu.Lock()
	status, ok := e.statuses[evseUID]
	e.mu.Unlock()
	if !ok {
		status = fallback
	}
	return ocpiEVSEStatus(status)
}

// ocpiStandard maps a canonical connector standard back onto its OCPI name — the
// inverse of the receiver's mapping (ocpi.canonicalStandard, OCPI receivers).
func ocpiStandard(canonical string) string {
	switch strings.ToUpper(strings.TrimSpace(canonical)) {
	case store.StandardTYPE2:
		return "IEC_62196_T2"
	case store.StandardCCS2:
		return "IEC_62196_T2_COMBO"
	case store.StandardCHADEMO:
		return "CHADEMO"
	case store.StandardCCS1:
		return "IEC_62196_T1_COMBO"
	case store.StandardGBT:
		return "GBT_DC"
	case store.StandardNACS:
		return "TESLA"
	default:
		return canonical
	}
}

// ocpiEVSEStatus maps a canonical status back onto an OCPI EVSE status — the
// inverse of the receiver's mapping (ocpi.canonicalStatus).
func ocpiEVSEStatus(canonical string) string {
	switch strings.ToUpper(strings.TrimSpace(canonical)) {
	case store.StatusAvailable:
		return ocpi.EVSEAvailable
	case store.StatusCharging:
		return ocpi.EVSECharging
	case store.StatusBlocked:
		return ocpi.EVSEBlocked
	case store.StatusInoperative:
		return ocpi.EVSEInoperative
	default:
		return ocpi.EVSEAvailable
	}
}

// connectorID names one connector inside its EVSE: the OCPI id it was stored
// under, or its position when the CPO never gave one.
func connectorID(c store.OCPIConnectorRead, index int) string {
	if id := strings.TrimSpace(c.OCPIConnectorID); id != "" {
		return id
	}
	return strconv.Itoa(index + 1)
}

// tariffIDs renders the single tariff a connector references, or nothing.
func tariffIDs(id string) []string {
	if strings.TrimSpace(id) == "" {
		return nil
	}
	return []string{id}
}

// nowRFC3339 is the timestamp OCPI documents carry (UTC, whole seconds).
func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

func wirePowerType(v string) string {
	if v == "AC" {
		return "AC_3_PHASE"
	}
	return "DC"
}
