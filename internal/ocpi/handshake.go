package ocpi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/chargenet/chargenet-backend/internal/store"
)

// ChargeNet-initiated handshake verification. Registration
// only proves a CPO *called* ChargeNet; it says nothing about whether the OCPI
// endpoints ChargeNet was told about actually answer. [Handshake] is the other
// direction: the same calls the pull sync makes (versions → version details →
// locations → tariffs), each recorded as an ordered step, and on success the
// CPO's network is upserted through the receiver's own code path.

// Step statuses of a handshake log line.
const (
	StepPass    = "PASS"
	StepFail    = "FAIL"
	StepSkipped = "SKIPPED"
)

// HandshakeStep is one line of the ordered verification log the portal renders.
type HandshakeStep struct {
	Step       int    `json:"step"`
	Label      string `json:"label"`
	Status     string `json:"status"`
	Detail     string `json:"detail"`
	DurationMs int64  `json:"durationMs"`
}

// HandshakeResult is a whole verification: the ordered steps and the status the
// CPO row should carry afterwards (VERIFIED when no required step failed).
type HandshakeResult struct {
	Status string          `json:"status"`
	Steps  []HandshakeStep `json:"steps"`
}

// Handshake verifies a CPO's OCPI endpoints. The first four steps are required;
// the last two are optional, so a partner that serves locations but not tariffs
// — which is legal — still verifies. The caller persists the outcome with
// [Store.SetHandshakeResult]; [RunHandshake] does both.
func Handshake(ctx context.Context, st *store.Store, cpo *store.CpoAccount, client *Client) HandshakeResult {
	if client == nil {
		client = NewClient()
	}
	h := &handshaker{ctx: ctx, st: st, cpo: cpo, client: client}

	h.step("Credentials registered", h.credentials)
	h.step("GET versions", h.versions)
	h.step("GET version details", h.details)
	h.step("GET locations", h.locations)
	h.step("GET tariffs", h.tariffs)
	h.step("Commands endpoint reachable", h.commands)

	status := store.HandshakeVerified
	for _, s := range h.steps {
		if s.Status == StepFail {
			status = store.HandshakeFailed
			break
		}
	}
	return HandshakeResult{Status: status, Steps: h.steps}
}

// RunHandshake verifies a CPO and writes the result back to its row: the status
// and the JSON step log. It is what the portal's handshake-test endpoint calls.
func RunHandshake(ctx context.Context, st *store.Store, cpo *store.CpoAccount) (HandshakeResult, error) {
	res := Handshake(ctx, st, cpo, NewClient())
	log, err := json.Marshal(res.Steps)
	if err != nil {
		return res, err
	}
	if err := st.SetHandshakeResult(ctx, cpo.ID, res.Status, log); err != nil {
		return res, err
	}
	return res, nil
}

// handshaker accumulates the steps as it runs them.
type handshaker struct {
	ctx    context.Context
	st     *store.Store
	cpo    *store.CpoAccount
	client *Client
	steps  []HandshakeStep
}

// step runs one check, timing it and appending the outcome.
func (h *handshaker) step(label string, fn func() (status, detail string)) {
	start := time.Now()
	status, detail := fn()
	h.steps = append(h.steps, HandshakeStep{
		Step:       len(h.steps) + 1,
		Label:      label,
		Status:     status,
		Detail:     detail,
		DurationMs: time.Since(start).Milliseconds(),
	})
}

// credentials is step 1: ChargeNet can only call a CPO it has credentials for.
func (h *handshaker) credentials() (string, string) {
	if strings.TrimSpace(h.cpo.TokenB) == "" || strings.TrimSpace(h.cpo.TokenC) == "" {
		return StepFail, "ChargeNet holds no Token B/Token C for this CPO. The CPO must call ChargeNet's /ocpi/emsp/versions with its Token A first."
	}
	if strings.TrimSpace(h.cpo.BaseURL) == "" {
		return StepFail, "The CPO has no OCPI base URL yet. Save one on the OCPI endpoint step and try again."
	}
	return StepPass, "Token B and Token C are registered."
}

// versions is step 2: `GET {base_url}/versions` must advertise OCPI 2.3.0.
func (h *handshaker) versions() (string, string) {
	data, err := h.client.Do(h.ctx, h.cpo, http.MethodGet, "/versions", nil)
	if err != nil {
		return StepFail, "Calling " + h.cpo.BaseURL + "/versions failed: " + err.Error()
	}
	var list []Version
	if err := json.Unmarshal(data, &list); err != nil {
		return StepFail, "The versions response was not an OCPI list: " + err.Error()
	}
	for _, v := range list {
		if v.Version == VersionNumber {
			h.cpo.OCPIVersion = VersionNumber
			if h.st != nil {
				if _, err := h.st.DB.ExecContext(h.ctx, `UPDATE cpo_partners SET ocpi_version=$2 WHERE id=$1`, h.cpo.ID, VersionNumber); err != nil {
					return StepFail, err.Error()
				}
			}
			return StepPass, "The CPO serves OCPI " + VersionNumber + "."
		}
	}
	return StepFail, "The CPO does not list a supported OCPI version."
}

// details is step 3: `GET {base_url}/2.3.0/` must expose the locations and
// commands modules ChargeNet needs to charge a driver.
func (h *handshaker) details() (string, string) {
	path := "/" + VersionNumber + "/"
	data, err := h.client.Do(h.ctx, h.cpo, http.MethodGet, path, nil)
	if err != nil {
		return StepFail, "Calling " + h.cpo.BaseURL + path + " failed: " + err.Error()
	}
	var d VersionDetails
	if err := json.Unmarshal(data, &d); err != nil {
		return StepFail, "The version-details response was not an OCPI object: " + err.Error()
	}
	if d.Version != VersionNumber {
		return StepFail, "The CPO returned the wrong OCPI version details."
	}
	has := func(module string) bool {
		for _, e := range d.Endpoints {
			if e.Identifier == module {
				return true
			}
		}
		return false
	}
	missing := []string{}
	for _, m := range []string{ModuleLocations, ModuleCommands} {
		if !has(m) {
			missing = append(missing, m)
		}
	}
	if len(missing) > 0 {
		return StepFail, "The endpoint list is missing: " + strings.Join(missing, ", ") + "."
	}
	endpoints := map[string]string{}
	for _, endpoint := range d.Endpoints {
		parsed, err := url.Parse(endpoint.URL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return StepFail, "The endpoint URL must be absolute HTTP(S)."
		}
		endpoints[endpoint.Identifier] = endpoint.URL
	}
	h.cpo.ModuleEndpoints = endpoints
	if h.st != nil {
		raw, _ := json.Marshal(endpoints)
		if _, err := h.st.DB.ExecContext(h.ctx, `UPDATE cpo_partners SET module_endpoints=$2::jsonb WHERE id=$1`, h.cpo.ID, string(raw)); err != nil {
			return StepFail, err.Error()
		}
	}
	return StepPass, strconv.Itoa(len(d.Endpoints)) + " endpoints, including locations and commands."
}

// locations is step 4: read the CPO's locations, count them, and store them. A
// location the receiver would refuse is skipped exactly as a push would skip it,
// so verification can never write something a push could not.
func (h *handshaker) locations() (string, string) {
	data, err := h.client.Do(h.ctx, h.cpo, http.MethodGet, locationsPath, nil)
	if err != nil {
		return StepFail, "Calling " + h.cpo.BaseURL + locationsPath + " failed: " + err.Error()
	}
	locs, err := decodeList[Location](data)
	if err != nil {
		return StepFail, "Reading the locations list failed: " + err.Error()
	}

	evses, stored := 0, 0
	for _, loc := range locs {
		evses += len(loc.EVSEs)
		in, err := locationInput(h.cpo, loc.ID, loc)
		if err != nil {
			continue
		}
		if _, err := h.st.UpsertOCPILocation(h.ctx, *in); err != nil {
			return StepFail, "Storing location " + loc.ID + " failed: " + err.Error()
		}
		stored++
	}
	return StepPass, fmt.Sprintf("%d locations, %d EVSEs; %d stored.", len(locs), evses, stored)
}

// tariffs is step 5 and optional: a CPO that does not serve the module is not an
// error.
func (h *handshaker) tariffs() (string, string) {
	data, err := h.client.Do(h.ctx, h.cpo, http.MethodGet, tariffsPath, nil)
	if err != nil {
		return StepSkipped, "The CPO does not serve the tariffs module (" + err.Error() + ")."
	}
	tariffs, err := decodeList[Tariff](data)
	if err != nil {
		return StepFail, "Reading the tariffs list failed: " + err.Error()
	}
	stored := 0
	for _, t := range tariffs {
		if err := checkTariff(t); err != nil {
			if e := h.st.InvalidateOCPITariff(h.ctx, h.cpo.ID, t.ID, t.RawJSON); e != nil {
				return StepFail, e.Error()
			}
			continue
		}
		in := store.OCPITariffInput{
			CpoID:       h.cpo.ID,
			ID:          t.ID,
			Currency:    strings.TrimSpace(t.Currency),
			PricePerKWh: firstEnergyPrice(t),
			Unsupported: !simpleEnergyTariff(t), RawJSON: t.RawJSON,
		}
		if err := h.st.UpsertOCPITariff(h.ctx, in); err != nil {
			return StepFail, "Storing tariff " + t.ID + " failed: " + err.Error()
		}
		stored++
	}
	return StepPass, strconv.Itoa(stored) + " tariffs stored."
}

// commands is an optional probe. Unsupported endpoints are SKIPPED because the
// first real command reports its own error.
func (h *handshaker) commands() (string, string) {
	if _, err := h.client.Do(h.ctx, h.cpo, http.MethodGet, CommandsPath, nil); err != nil {
		return StepSkipped, "The commands endpoint did not answer (" + err.Error() + ")."
	}
	return StepPass, "The commands endpoint answers."
}
