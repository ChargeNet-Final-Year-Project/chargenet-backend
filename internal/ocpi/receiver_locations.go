package ocpi

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/chargenet/chargenet-backend/internal/store"
)

// handleLocations serves the locations module: a CPO pushes its
// locations, ChargeNet stores them as stations and connectors.
//
// PUT carries a whole location and replaces what ChargeNet holds for it. PATCH
// carries the EVSE statuses that changed and comes in the two
// shapes the spec allows: one EVSE, named in the URL
// (`/locations/{cc}/{pid}/{loc}/{evse}`), or a partial Location naming them in
// its body. Both are authenticated with Token C and both must push the party
// that token belongs to.
func (s *Server) handleLocations(w http.ResponseWriter, r *http.Request) {
	parts, ok := s.pushPath(w, r, ModuleLocations)
	if !ok {
		return
	}
	locationID := parts[2]
	evseUID := ""
	if len(parts) == 4 {
		evseUID = parts[3]
	}

	switch {
	case r.Method == http.MethodPut && evseUID == "":
		s.putLocation(w, r, locationID)
	case r.Method == http.MethodPatch:
		s.patchLocation(w, r, locationID, evseUID)
	case evseUID != "":
		// An EVSE is never pushed on its own: it arrives inside its location.
		Fail(w, http.StatusNotFound, StatusClientError, "Unknown OCPI endpoint.")
	default:
		s.notServed(w, ModuleLocations)
	}
}

// putLocation ingests a full Location push.
func (s *Server) putLocation(w http.ResponseWriter, r *http.Request, locationID string) {
	cpo := CPOFrom(r.Context())
	push, ok := decodePush[Location](w, r, "location")
	if !ok {
		return
	}
	if push.Object.ID != "" && push.Object.ID != locationID {
		Fail(w, http.StatusBadRequest, StatusClientError,
			"The location id in the body is not the one in the URL.")
		return
	}

	in, err := locationInput(cpo, locationID, push.Object)
	if err != nil {
		Fail(w, http.StatusBadRequest, StatusClientError, "The location was refused: "+err.Error()+".")
		return
	}

	stationID, err := s.Store.UpsertOCPILocation(r.Context(), *in)
	if err != nil {
		s.failStore(w, r, "location", err)
		return
	}
	log.Printf("ocpi: partner=%s stored location %s as station %s with %d connectors",
		cpo.ID, locationID, stationID, len(in.Connectors))

	push.Object.ID = locationID
	OK(w, http.StatusOK, push.Object)
}

// patchLocation applies a Location PATCH: the EVSE statuses a CPO reports
// changed.
//
// The body is a partial Location — some CPOs send the whole document, some only
// the `evse` array, and the EVSE-level URL sends one EVSE — so nothing but the
// statuses is read: a PATCH must never replace a location's connectors, which is
// what a PUT means.
func (s *Server) patchLocation(w http.ResponseWriter, r *http.Request, locationID, evseUID string) {
	cpo := CPOFrom(r.Context())
	push, ok := decodePush[locationPatch](w, r, "location")
	if !ok {
		return
	}

	updates := push.Object.statuses(evseUID)
	if len(updates) == 0 {
		Fail(w, http.StatusBadRequest, StatusClientError, "The patch carried no EVSE status.")
		return
	}
	// Every status is read before any is written: a patch half applied because
	// its fourth EVSE was unreadable would leave the driver app showing a
	// location that is neither what the CPO sent nor what it was.
	statuses := make([]string, len(updates))
	for i, evse := range updates {
		status, known := canonicalStatus(evse.Status)
		if !known {
			Fail(w, http.StatusBadRequest, StatusClientError,
				fmt.Sprintf("The EVSE %s reports the status %q, which ChargeNet cannot show.", evse.UID, evse.Status))
			return
		}
		statuses[i] = status
	}

	for i, evse := range updates {
		if err := s.Store.SetOCPIConnectorStatus(r.Context(), cpo.ID, evse.UID, statuses[i]); err != nil {
			s.failStore(w, r, "location", err)
			return
		}
	}
	log.Printf("ocpi: partner=%s patched %d EVSE statuses of location %s",
		cpo.ID, len(updates), locationID)

	push.Object.ID = locationID
	OK(w, http.StatusOK, push.Object)
}

// locationPatch is what a Location PATCH can carry: the EVSEs whose status
// changed, or — when the URL names one EVSE — that EVSE on its own. The `uid`
// and `status` names stay the spec's.
type locationPatch struct {
	ID     string       `json:"id,omitempty"`
	UID    string       `json:"uid,omitempty"`
	Status string       `json:"status,omitempty"`
	EVSEs  []evseStatus `json:"evse,omitempty"`
}

// evseStatus is one EVSE of a location with the status a push reports for it.
type evseStatus struct {
	UID    string `json:"uid"`
	Status string `json:"status"`
}

// statuses is the list of EVSE statuses this patch applies.
//
// When the URL names an EVSE, that is the only one patched and the URL's uid is
// what the store knows the row by: the body is the EVSE object itself, or the
// location whose `evse` array holds it.
func (p locationPatch) statuses(evseUID string) []evseStatus {
	if evseUID == "" {
		return p.EVSEs
	}
	status := p.Status
	for _, evse := range p.EVSEs {
		if strings.EqualFold(evse.UID, evseUID) {
			status = evse.Status
		}
	}
	return []evseStatus{{UID: evseUID, Status: status}}
}

// locationInput translates OCPI standards, statuses, power and nesting into the
// store model. Unsupported values are rejected before they reach the driver app.
func locationInput(cpo *store.CpoAccount, locationID string, body Location) (*store.OCPILocationInput, error) {
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = locationID
	}
	address, city := strings.TrimSpace(body.Address), strings.TrimSpace(body.City)
	if address == "" || city == "" {
		return nil, fmt.Errorf("a location needs an address and a city")
	}

	latitude, err := strconv.ParseFloat(strings.TrimSpace(body.Coordinates.Latitude), 64)
	if err != nil {
		return nil, fmt.Errorf("the latitude %q is not in decimal degrees", body.Coordinates.Latitude)
	}
	longitude, err := strconv.ParseFloat(strings.TrimSpace(body.Coordinates.Longitude), 64)
	if err != nil {
		return nil, fmt.Errorf("the longitude %q is not in decimal degrees", body.Coordinates.Longitude)
	}
	if len(body.EVSEs) == 0 {
		return nil, fmt.Errorf("a location needs at least one EVSE")
	}

	country := CountryAlpha2(body.Country)
	if country == "" {
		country = cpo.CountryCode
	}
	if country != "MY" && country != "SG" && country != "TH" {
		return nil, fmt.Errorf("unsupported location country")
	}
	if latitude < -90 || latitude > 90 || longitude < -180 || longitude > 180 {
		return nil, fmt.Errorf("invalid coordinates")
	}
	in := &store.OCPILocationInput{
		CountryCode: country,
		CpoID:       cpo.ID,
		LocationID:  locationID,
		Name:        name,
		Address:     address,
		City:        city,
		Region:      strings.TrimSpace(body.State),
		Latitude:    latitude,
		Longitude:   longitude,
	}

	seen := map[string]bool{}
	for _, evse := range body.EVSEs {
		uid := strings.TrimSpace(evse.UID)
		if uid == "" {
			return nil, fmt.Errorf("an EVSE needs a uid")
		}
		if seen[uid] {
			return nil, fmt.Errorf("the EVSE %s is listed twice", uid)
		}
		seen[uid] = true
		if len(evse.Connectors) == 0 {
			return nil, fmt.Errorf("the EVSE %s carries no connector", uid)
		}

		status, known := canonicalStatus(evse.Status)
		if !known {
			return nil, fmt.Errorf("the EVSE %s reports the status %q, which ChargeNet cannot show", uid, evse.Status)
		}

		for i, c := range evse.Connectors {
			standard, known := canonicalStandard(c.Standard)
			if !known {
				return nil, fmt.Errorf("the connector %s uses the standard %q, which ChargeNet cannot show", c.ID, c.Standard)
			}
			connector := store.OCPIConnectorInput{
				EVSEUID:         EVSEUIDFor(uid, c.ID, i),
				OCPIConnectorID: c.ID,
				Standard:        standard,
				Format:          connectorFormat(c.Format),
				PowerType:       connectorPowerType(c.PowerType),
				MaxPowerKW:      connectorPowerKW(c),
				Status:          status,
			}
			if len(c.TariffIDs) > 0 {
				connector.TariffID = c.TariffIDs[0]
			}
			in.Connectors = append(in.Connectors, connector)
		}
	}
	return in, nil
}
