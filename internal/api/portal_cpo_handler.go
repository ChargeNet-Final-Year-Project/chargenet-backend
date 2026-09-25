package api

import (
	"encoding/csv"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/chargenet/chargenet-backend/internal/ocpi"
	"github.com/chargenet/chargenet-backend/internal/sessions"
	"github.com/chargenet/chargenet-backend/internal/store"
)

// registerPortalCPORoutes installs the CPO-role portal endpoints.
// Every handler here scopes its query by the token's cpo id — [CPOIDFrom] — and
// never by an id the client supplies, which is what makes tenancy hold.
func registerPortalCPORoutes(mux *http.ServeMux, s *Server) {
	cpo := func(h http.HandlerFunc) http.Handler {
		return Chain(h, s.RequirePortal(store.PortalRoleCPO))
	}
	mux.Handle("GET /admin/api/cpo", cpo(s.handlePortalGetCPO))
	mux.Handle("PUT /admin/api/cpo", cpo(s.handlePortalUpdateCPO))
	mux.Handle("POST /admin/api/cpo/token-a/regenerate", cpo(s.handlePortalRegenerateTokenA))
	mux.Handle("POST /admin/api/cpo/handshake-test", cpo(s.handlePortalHandshakeTest))
	mux.Handle("POST /admin/api/cpo/disconnect", cpo(s.handlePortalDisconnectCPO))
	mux.Handle("GET /admin/api/network", cpo(s.handlePortalNetwork))
	mux.Handle("GET /admin/api/overview", cpo(s.handlePortalOverview))
	mux.Handle("GET /admin/api/cdrs", cpo(s.handlePortalCDRs))
	mux.Handle("GET /admin/api/cdrs/{id}", cpo(s.handlePortalCDR))
	mux.Handle("POST /admin/api/sandbox/command", cpo(s.handlePortalSandboxCommand))
}

// maskAccountNo renders a bank account number for display: only the last four
// digits survive.
func maskAccountNo(no string) string {
	no = strings.TrimSpace(no)
	if len(no) <= 4 {
		return no
	}
	return strings.Repeat("•", len(no)-4) + no[len(no)-4:]
}

// handlePortalGetCPO returns the CPO's own profile with the bank account number
// masked. Token A is included while it is still usable.
func (s *Server) handlePortalGetCPO(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.GetCPOProfile(r.Context(), CPOIDFrom(r.Context()))
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, http.StatusNotFound, CodeForbidden, "No company profile for this account.")
		return
	}
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not load your company profile.")
		return
	}
	p.BankAccountNo = maskAccountNo(p.BankAccountNo)
	WriteJSON(w, http.StatusOK, p)
}

// portalUpdateCPORequest is the `PUT /admin/api/cpo` body.
type portalUpdateCPORequest struct {
	Name         string `json:"name"`
	BaseURL      string `json:"baseUrl"`
	ContactEmail string `json:"contactEmail"`
	ColorHex     string `json:"colorHex"`
	Bank         struct {
		BankName      string `json:"bankName"`
		AccountHolder string `json:"accountHolder"`
		AccountNo     string `json:"accountNo"`
	} `json:"bank"`
	PayoutCycle string `json:"payoutCycle"`
}

// handlePortalUpdateCPO writes the editable profile fields. The account number
// is only written when the console sends a real one: a masked value (which is
// what GET returns) means "unchanged".
func (s *Server) handlePortalUpdateCPO(w http.ResponseWriter, r *http.Request) {
	var body portalUpdateCPORequest
	if err := DecodeJSON(r, &body); err != nil {
		WriteError(w, http.StatusBadRequest, CodeValidation, "Malformed request body.")
		return
	}

	body.Name = strings.TrimSpace(body.Name)
	body.ContactEmail = strings.ToLower(strings.TrimSpace(body.ContactEmail))
	body.PayoutCycle = strings.ToUpper(strings.TrimSpace(body.PayoutCycle))
	accountNo := strings.TrimSpace(body.Bank.AccountNo)
	if strings.Contains(accountNo, "•") {
		accountNo = ""
	}

	fields := map[string]string{}
	if body.Name == "" {
		fields["name"] = "Company name is required."
	}
	if !emailRE.MatchString(body.ContactEmail) {
		fields["contactEmail"] = "Enter a valid email address."
	}
	if !containsString(store.PayoutCycles, body.PayoutCycle) {
		fields["payoutCycle"] = "Payout cycle must be WEEKLY or MONTHLY."
	}
	if accountNo != "" && !portalAccountRE.MatchString(accountNo) {
		fields["bank.accountNo"] = "Account number must be 8-20 digits."
	}
	if len(fields) > 0 {
		WriteFieldError(w, "Please fix the highlighted fields.", fields)
		return
	}

	cpoID := CPOIDFrom(r.Context())
	err := s.Store.UpdateCPOProfile(r.Context(), cpoID, store.CPOProfileUpdate{
		Name:              body.Name,
		BaseURL:           strings.TrimSpace(body.BaseURL),
		ContactEmail:      body.ContactEmail,
		ColorHex:          strings.TrimSpace(body.ColorHex),
		BankName:          strings.TrimSpace(body.Bank.BankName),
		BankAccountHolder: strings.TrimSpace(body.Bank.AccountHolder),
		BankAccountNo:     accountNo,
		PayoutCycle:       body.PayoutCycle,
	})
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, http.StatusNotFound, CodeForbidden, "No company profile for this account.")
		return
	}
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not save your profile.")
		return
	}

	p, err := s.Store.GetCPOProfile(r.Context(), cpoID)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not save your profile.")
		return
	}
	p.BankAccountNo = maskAccountNo(p.BankAccountNo)
	WriteJSON(w, http.StatusOK, p)
}

// handlePortalRegenerateTokenA issues a fresh Token A while the handshake has
// not been verified.
func (s *Server) handlePortalRegenerateTokenA(w http.ResponseWriter, r *http.Request) {
	tokenA, err := newTokenA()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not issue a new token.")
		return
	}
	err = s.Store.RegenerateTokenA(r.Context(), CPOIDFrom(r.Context()), tokenA)
	switch {
	case errors.Is(err, store.ErrHandshakeVerified):
		WriteError(w, http.StatusConflict, CodeForbidden,
			"The OCPI handshake is already verified; Token A can no longer be rotated.")
		return
	case errors.Is(err, store.ErrNotFound):
		WriteError(w, http.StatusNotFound, CodeForbidden, "No company profile for this account.")
		return
	case err != nil:
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not issue a new token.")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]string{"tokenA": tokenA})
}

// handlePortalHandshakeTest verifies the CPO's OCPI endpoints and persists the
// result.
func (s *Server) handlePortalHandshakeTest(w http.ResponseWriter, r *http.Request) {
	cpo, err := s.Store.GetCPO(r.Context(), CPOIDFrom(r.Context()))
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, http.StatusNotFound, CodeForbidden, "No company profile for this account.")
		return
	}
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not run the handshake test.")
		return
	}
	res, err := ocpi.RunHandshake(r.Context(), s.Store, cpo)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not record the handshake result.")
		return
	}
	WriteJSON(w, http.StatusOK, res)
}

// handlePortalDisconnectCPO clears the partner's stored OCPI credentials and
// returns it to the pre-handshake state. It is the portal's
// stand-in for the OCPI credentials DELETE: the partner stays registered and
// keeps its network and billing, but ChargeNet can no longer call it until a
// fresh handshake succeeds.
func (s *Server) handlePortalDisconnectCPO(w http.ResponseWriter, r *http.Request) {
	err := s.Store.DisconnectCPO(r.Context(), CPOIDFrom(r.Context()))
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, http.StatusNotFound, CodeForbidden, "No company profile for this account.")
		return
	}
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not disconnect your platform.")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"handshakeStatus": store.HandshakeNotStarted})
}

// handlePortalNetwork returns the CPO's stations, bays and today's session count.
func (s *Server) handlePortalNetwork(w http.ResponseWriter, r *http.Request) {
	view, err := s.Store.NetworkForCPO(r.Context(), CPOIDFrom(r.Context()))
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not load your network.")
		return
	}
	WriteJSON(w, http.StatusOK, view)
}

// handlePortalOverview returns the CPO dashboard KPIs.
func (s *Server) handlePortalOverview(w http.ResponseWriter, r *http.Request) {
	view, err := s.Store.OverviewForCPO(r.Context(), CPOIDFrom(r.Context()), reportCurrencyParam(r))
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not load your overview.")
		return
	}
	WriteJSON(w, http.StatusOK, view)
}

// handlePortalCDRs returns the CPO's CDR ledger page and range totals, or the
// whole range as CSV when `format=csv`.
func (s *Server) handlePortalCDRs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, to, err := reportingRange(q.Get("from"), q.Get("to"), time.Now())
	if err != nil {
		WriteError(w, http.StatusBadRequest, CodeValidation, "Use valid YYYY-MM-DD dates with From on or before To (Asia/Kuala_Lumpur).")
		return
	}

	limit := atoiDefault(q.Get("limit"), 25)
	if limit < 1 || limit > 500 {
		limit = 25
	}
	page := atoiDefault(q.Get("page"), 1)
	if page < 1 {
		page = 1
	}

	if strings.EqualFold(q.Get("format"), "csv") {
		s.exportLedgerCSV(w, r, strings.TrimSpace(q.Get("evse")), from, to)
		return
	}
	rows, totals, err := s.Store.CDRLedger(r.Context(), CPOIDFrom(r.Context()), strings.TrimSpace(q.Get("evse")), from, to, limit, (page-1)*limit, reportCurrencyParam(r))
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not load the ledger.")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"from": from, "to": to, "page": page, "limit": limit,
		"rows": rows, "totals": totals, "evseUid": strings.TrimSpace(q.Get("evse")),
		"reportingTimezone": store.ReportingTimezone, "order": "settled_desc",
	})
}

// handlePortalCDR returns one CDR: the ledger row plus the stored OCPI document
// the console pretty-prints. Tenancy is enforced by the
// store, which scopes the read to the caller's own cpo id.
func (s *Server) handlePortalCDR(w http.ResponseWriter, r *http.Request) {
	cdr, err := s.Store.CDRDetail(r.Context(), CPOIDFrom(r.Context()), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, http.StatusNotFound, CodeValidation, "No such charge detail record.")
		return
	}
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not load the record.")
		return
	}
	WriteJSON(w, http.StatusOK, cdr)
}

// Inclusive calendar dates become a half-open instant range in the reporting zone.
func reportingRange(fromValue, toValue string, now time.Time) (time.Time, time.Time, error) {
	local := now.In(store.ReportingLocation)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, store.ReportingLocation)
	parse := func(value string, fallback time.Time) (time.Time, error) {
		if strings.TrimSpace(value) == "" {
			return fallback, nil
		}
		return time.ParseInLocation("2006-01-02", strings.TrimSpace(value), store.ReportingLocation)
	}
	from, err := parse(fromValue, today.AddDate(0, 0, -29))
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	to, err := parse(toValue, today)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if from.After(to) {
		return time.Time{}, time.Time{}, errors.New("reversed reporting range")
	}
	return from, to.AddDate(0, 0, 1), nil
}

func atoiDefault(value string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return def
	}
	return n
}

// Build the complete file before sending headers so failures cannot return a
// successful partial financial export.
func (s *Server) exportLedgerCSV(w http.ResponseWriter, r *http.Request, evse string, from, to time.Time) {
	file, err := os.CreateTemp("", "chargenet-cdrs-*.csv")
	if err != nil {
		WriteError(w, 500, CodeInternal, "Could not prepare the export.")
		return
	}
	defer func() { file.Close(); os.Remove(file.Name()) }()
	cw := csv.NewWriter(file)
	err = cw.Write([]string{"cdr_id", "session_id", "start", "end", "station", "evse",
		"kwh", "minutes", "tariff", "gross", "platform_fee", "net_payout", "currency"})
	if err != nil {
		WriteError(w, 500, CodeInternal, "Could not prepare the export.")
		return
	}
	totals, err := s.Store.ExportCDRLedger(r.Context(), CPOIDFrom(r.Context()), evse, from, to, func(row store.LedgerRow) error {
		return cw.Write([]string{row.CDRID, row.SessionID, row.StartTime.Format(time.RFC3339), row.EndTime.Format(time.RFC3339),
			row.StationName, row.EVSEUID, strconv.FormatFloat(row.EnergyKWh, 'f', 3, 64), strconv.Itoa(row.Minutes),
			strconv.FormatFloat(row.TariffPerKWh, 'f', 2, 64), strconv.FormatFloat(row.Gross, 'f', 2, 64),
			strconv.FormatFloat(row.Fee, 'f', 2, 64), strconv.FormatFloat(row.Net, 'f', 2, 64), row.Currency})
	}, reportCurrencyParam(r))
	if err == nil {
		err = cw.Write([]string{"TOTAL", "", "", "", "", "", strconv.FormatFloat(totals.EnergyKWh, 'f', 3, 64), "", "",
			strconv.FormatFloat(totals.Gross, 'f', 2, 64), strconv.FormatFloat(totals.Fee, 'f', 2, 64),
			strconv.FormatFloat(totals.Net, 'f', 2, 64), totals.Currency})
	}
	cw.Flush()
	if err == nil {
		err = cw.Error()
	}
	if err == nil {
		_, err = file.Seek(0, io.SeekStart)
	}
	if err != nil {
		WriteError(w, 500, CodeInternal, "Could not export the complete ledger. Please retry.")
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="chargenet-cdrs.csv"`)
	_, _ = io.Copy(w, file)
}

// portalSandboxRequest is the `POST /admin/api/sandbox/command` body.
type portalSandboxRequest struct {
	EVSEUID   string  `json:"evseUid"`
	Command   string  `json:"command"`
	TargetSoC float64 `json:"targetSoc"`
}

// handlePortalSandboxCommand drives a real charge command against one of the
// caller's own EVSEs, billed to the internal sandbox driver.
// Those sessions are excluded from every revenue report by the store queries.
func (s *Server) handlePortalSandboxCommand(w http.ResponseWriter, r *http.Request) {
	var body portalSandboxRequest
	if err := DecodeJSON(r, &body); err != nil {
		WriteError(w, http.StatusBadRequest, CodeValidation, "Malformed request body.")
		return
	}
	body.EVSEUID = strings.TrimSpace(body.EVSEUID)
	command := strings.ToUpper(strings.TrimSpace(body.Command))

	// Tenancy: resolve the EVSE and require it to be the caller's own. GetEVSE
	// applies the same ACTIVE-partner gate the driver path uses.
	station, conn, err := s.Store.GetEVSE(r.Context(), body.EVSEUID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		WriteError(w, http.StatusNotFound, CodeEVSENotFound, "No such EVSE.")
		return
	case errors.Is(err, store.ErrCpoInactive), station != nil && station.CpoID != CPOIDFrom(r.Context()):
		WriteError(w, http.StatusForbidden, CodeForbidden, "That EVSE is not on your network.")
		return
	case err != nil:
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not resolve the EVSE.")
		return
	}

	switch command {
	case "START_SESSION":
		quote, err := s.Store.CreateQuote(r.Context(), store.SandboxUserID, body.EVSEUID, "15")
		if err != nil {
			writeQuoteError(w, err)
			return
		}
		sess, err := s.Sessions.Start(r.Context(), store.SandboxUserID, sessions.StartRequest{
			EVSEUID: body.EVSEUID, TargetSoC: body.TargetSoC, QuoteID: quote.ID,
		})
		if err != nil {
			writeSessionError(w, err, "The CPO refused the command.")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"result": "ACCEPTED", "command": command, "evseUid": body.EVSEUID,
			"sessionId": sess.ID, "connectorId": conn.ID,
		})
	case "STOP_SESSION":
		active, err := s.Sessions.Active(r.Context(), store.SandboxUserID)
		if errors.Is(err, store.ErrNotFound) {
			WriteError(w, http.StatusConflict, CodeNoActiveSession, "No sandbox session is running.")
			return
		}
		if err != nil {
			WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not read the sandbox session.")
			return
		}
		sess, err := s.Sessions.Stop(r.Context(), store.SandboxUserID, active.ID)
		if err != nil {
			writeSessionError(w, err, "The CPO refused the command.")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"result": "ACCEPTED", "command": command, "evseUid": body.EVSEUID, "sessionId": sess.ID,
		})
	case "UNLOCK_CONNECTOR":
		if err := s.Gateway.UnlockConnector(r.Context(), body.EVSEUID); err != nil {
			writeSessionError(w, err, "The CPO refused the command.")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"result": "ACCEPTED", "command": command, "evseUid": body.EVSEUID,
		})
	default:
		WriteError(w, http.StatusBadRequest, CodeValidation,
			"Command must be START_SESSION, STOP_SESSION or UNLOCK_CONNECTOR.")
	}
}

func reportCurrencyParam(r *http.Request) string {
	c := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("currency")))
	if store.ValidCurrency(c) {
		return c
	}
	return "MYR"
}
