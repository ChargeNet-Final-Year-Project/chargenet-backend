package ocpi

import (
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/chargenet/chargenet-backend/internal/store"
)

// handleTariffs serves the tariffs module: a CPO pushes the prices its
// locations reference.
//
// PUT and PATCH are handled alike — both carry the CPO's current view of a
// tariff (ChargeNet stores the whole document either way) — because the store's
// per-kWh model has no partial update to apply.
func (s *Server) handleTariffs(w http.ResponseWriter, r *http.Request) {
	parts, ok := s.pushPath(w, r, ModuleTariffs)
	if !ok {
		return
	}
	if len(parts) != 3 {
		Fail(w, http.StatusNotFound, StatusClientError, "Unknown OCPI endpoint.")
		return
	}
	switch r.Method {
	case http.MethodPut, http.MethodPatch:
		s.putTariff(w, r, parts[2])
	default:
		s.notServed(w, ModuleTariffs)
	}
}

// putTariff ingests one tariff and reprices every connector that lists it: a CPO that changes a price must not have to re-push its
// locations for the driver app to see it.
func (s *Server) putTariff(w http.ResponseWriter, r *http.Request, tariffID string) {
	cpo := CPOFrom(r.Context())
	push, ok := decodePush[Tariff](w, r, "tariff")
	if !ok {
		return
	}
	if push.Object.ID != "" && push.Object.ID != tariffID {
		Fail(w, http.StatusBadRequest, StatusClientError,
			"The tariff id in the body is not the one in the URL.")
		return
	}

	if err := checkTariff(push.Object); err != nil {
		if storeErr := s.Store.InvalidateOCPITariff(r.Context(), cpo.ID, tariffID, push.Raw); storeErr != nil {
			s.failStore(w, r, "tariff", storeErr)
			return
		}
		Fail(w, http.StatusBadRequest, StatusClientError, "The tariff was refused: "+err.Error()+".")
		return
	}

	in := store.OCPITariffInput{
		CpoID:       cpo.ID,
		ID:          tariffID,
		Currency:    strings.TrimSpace(push.Object.Currency),
		PricePerKWh: firstEnergyPrice(push.Object),
		Unsupported: !simpleEnergyTariff(push.Object), RawJSON: push.Raw,
	}
	if err := s.Store.UpsertOCPITariff(r.Context(), in); err != nil {
		s.failStore(w, r, "tariff", err)
		return
	}
	log.Printf("ocpi: partner=%s stored tariff %s at %.2f %s per kWh",
		cpo.ID, tariffID, in.PricePerKWh, in.Currency)

	push.Object.ID = tariffID
	OK(w, http.StatusOK, push.Object)
}

// checkTariff refuses the two tariffs ChargeNet cannot bill with.
//
// The tariff model uses a single per-kWh price, so a tariff with no
// ENERGY component has nothing to store, and the ledger bills in MYR: storing a
// tariff in another currency would put a number in the driver's cost column that
// the wallet cannot pay.
func checkTariff(t Tariff) error {
	if firstEnergyPrice(t) <= 0 {
		return errors.New("the tariff has no ENERGY price component")
	}
	if currency := strings.TrimSpace(t.Currency); currency != "" && !store.ValidCurrency(strings.ToUpper(currency)) {
		return errors.New("unsupported tariff currency " + currency)
	}
	return nil
}

// Keep unsupported tariff details for inspection but prevent quotation.
func simpleEnergyTariff(t Tariff) bool {
	if len(t.Elements) != 1 || len(t.Elements[0].PriceComponents) != 1 || len(t.Elements[0].Restrictions) > 0 {
		return false
	}
	if t.StartDateTime != "" || t.EndDateTime != "" || len(t.MinPrice) > 0 || len(t.MaxPrice) > 0 {
		return false
	}
	p := t.Elements[0].PriceComponents[0]
	return p.Type == "ENERGY" && p.StepSize == 1 && (p.VAT == nil || *p.VAT == 0) && (t.TaxIncluded == "" || t.TaxIncluded == "N/A")
}
