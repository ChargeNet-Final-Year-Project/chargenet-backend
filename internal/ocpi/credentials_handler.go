package ocpi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/chargenet/chargenet-backend/internal/store"
)

// maxCredentialsBody caps the credentials body. It is a few hundred bytes in
// practice; the limit exists so a hostile or broken client cannot stream
// forever into memory.
const maxCredentialsBody = 64 << 10

// handlePostCredentials completes the registration exchange.
//
// The CPO presents the Token A ChargeNet gave it and hands over what ChargeNet
// needs to call it back: its own token (stored as Token B) and its versions URL
// (reduced to the OCPI root). ChargeNet answers with Token C — the credential
// the CPO must use from now on — and invalidates Token A in the same write, so a
// replay of this request is rejected.
func (s *Server) handlePostCredentials(w http.ResponseWriter, r *http.Request) {
	cpo := CPOFrom(r.Context())

	body, ok := decodeCredentials(w, r)
	if !ok {
		return
	}
	baseURL, err := validateCredentials(body, cpo)
	if err != nil {
		Fail(w, http.StatusBadRequest, StatusClientError, "Invalid credentials: "+err.Error()+".")
		return
	}
	tokenC, err := NewToken()
	if err != nil {
		log.Printf("ocpi: minting Token C for %s: %v", cpo.ID, err)
		Fail(w, http.StatusInternalServerError, StatusUnableToUseAPI, "Could not issue a token.")
		return
	}

	tokenB := strings.TrimSpace(body.Token)
	if err := s.Store.CompleteHandshake(r.Context(), cpo.ID, cpo.TokenA, tokenB, baseURL, tokenC); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			Fail(w, http.StatusUnauthorized, StatusUnauthorized, "The registration token was consumed, rotated, or suspended.")
			return
		}
		log.Printf("ocpi: storing credentials for %s: %v", cpo.ID, err)
		Fail(w, http.StatusInternalServerError, StatusUnableToUseAPI, "Could not store the credentials.")
		return
	}

	if _, err := s.Store.DB.ExecContext(r.Context(), `UPDATE cpo_partners SET ocpi_version=$2 WHERE id=$1`, cpo.ID, VersionNumber); err != nil {
		Fail(w, 500, StatusUnableToUseAPI, "Could not retain the negotiated version.")
		return
	}
	log.Printf("ocpi: %s registered (party=%s/%s base_url=%s handshake=%s)",
		cpo.ID, cpo.CountryCode, cpo.PartyID, baseURL, store.HandshakeInProgress)
	OK(w, http.StatusOK, s.credentials(tokenC))
}

// handleGetCredentials returns ChargeNet's credentials to the registered CPO. The
// token is the one it already holds, so a client can re-read its configuration
// without rotating anything.
func (s *Server) handleGetCredentials(w http.ResponseWriter, r *http.Request) {
	OK(w, http.StatusOK, s.credentials(CPOFrom(r.Context()).TokenC))
}

// handlePutCredentials rotates the pair: the CPO replaced its Token B and
// ChargeNet answers with a fresh Token C. Both halves are stored, so the new
// Token B is what ChargeNet will call the CPO with from the next command on.
func (s *Server) handlePutCredentials(w http.ResponseWriter, r *http.Request) {
	cpo := CPOFrom(r.Context())

	body, ok := decodeCredentials(w, r)
	if !ok {
		return
	}
	baseURL, err := validateCredentials(body, cpo)
	if err != nil {
		Fail(w, http.StatusBadRequest, StatusClientError, "Invalid credentials: "+err.Error()+".")
		return
	}
	tokenC, err := NewToken()
	if err != nil {
		log.Printf("ocpi: minting Token C for %s: %v", cpo.ID, err)
		Fail(w, http.StatusInternalServerError, StatusUnableToUseAPI, "Could not issue a token.")
		return
	}

	tokenB := strings.TrimSpace(body.Token)
	if err := s.Store.UpdateCPOCredentials(r.Context(), cpo.ID, tokenB, baseURL, tokenC); err != nil {
		log.Printf("ocpi: rotating credentials for %s: %v", cpo.ID, err)
		Fail(w, http.StatusInternalServerError, StatusUnableToUseAPI, "Could not store the credentials.")
		return
	}

	if _, err := s.Store.DB.ExecContext(r.Context(), `UPDATE cpo_partners SET ocpi_version=$2 WHERE id=$1`, cpo.ID, VersionNumber); err != nil {
		Fail(w, 500, StatusUnableToUseAPI, "Could not retain the negotiated version.")
		return
	}
	log.Printf("ocpi: %s rotated credentials (party=%s/%s base_url=%s)",
		cpo.ID, cpo.CountryCode, cpo.PartyID, baseURL)
	OK(w, http.StatusOK, s.credentials(tokenC))
}

// handleDeleteCredentials unregisters the CPO: its tokens are dropped and its
// status becomes SUSPENDED, which hides its stations from the driver app. A
// partner that wants back in needs a new Token A from the portal.
func (s *Server) handleDeleteCredentials(w http.ResponseWriter, r *http.Request) {
	cpo := CPOFrom(r.Context())

	if err := s.Store.SuspendCPO(r.Context(), cpo.ID); err != nil {
		log.Printf("ocpi: unregistering %s: %v", cpo.ID, err)
		Fail(w, http.StatusInternalServerError, StatusUnableToUseAPI, "Could not unregister.")
		return
	}

	log.Printf("ocpi: %s unregistered (status=%s)", cpo.ID, store.CPOStatusSuspended)
	OK(w, http.StatusOK, map[string]any{})
}

// decodeCredentials reads a credentials body, answering the request itself when
// the body cannot be used. Unknown fields are tolerated on purpose: the spec
// grows, and a CPO sending a field ChargeNet does not model is not an error.
func decodeCredentials(w http.ResponseWriter, r *http.Request) (*Credentials, bool) {
	var body Credentials
	if err := json.NewDecoder(io.LimitReader(r.Body, maxCredentialsBody)).Decode(&body); err != nil {
		Fail(w, http.StatusBadRequest, StatusClientError, "Could not read the credentials body.")
		return nil, false
	}
	return &body, true
}

// validateCredentials checks the three things ChargeNet must trust before it
// stores them: the token it will send to the CPO, the versions URL it will call,
// and that the party registering is the one that owns the presented Token A. It
// returns the CPO's OCPI root.
func validateCredentials(body *Credentials, cpo *store.CpoAccount) (string, error) {
	if strings.TrimSpace(body.Token) == "" {
		return "", errors.New("token is required")
	}
	baseURL, err := versionsRoot(body.URL)
	if err != nil {
		return "", err
	}
	for _, role := range body.Roles {
		if strings.EqualFold(role.PartyID, cpo.PartyID) && strings.EqualFold(role.CountryCode, cpo.CountryCode) {
			return baseURL, nil
		}
	}
	return "", fmt.Errorf("no role matches party %s/%s", strings.ToUpper(cpo.CountryCode), strings.ToUpper(cpo.PartyID))
}

// versionsRoot derives the CPO's OCPI root from the versions URL it registers
// with: "http://host/emulator/voltgo/ocpi/versions" becomes
// "http://host/emulator/voltgo/ocpi", which is what the other module URLs are
// built from.
func versionsRoot(rawURL string) (string, error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return "", errors.New("url is required")
	}
	root := trimTrailingSlash(strings.TrimSuffix(trimTrailingSlash(trimmed), "/versions"))
	if root == "" || !strings.Contains(root, "://") {
		return "", fmt.Errorf("url %q is not an absolute versions URL", trimmed)
	}
	return root, nil
}
