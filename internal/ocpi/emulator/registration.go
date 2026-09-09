package emulator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/chargenet/chargenet-backend/internal/ocpi"
)

// Run starts the emulator's background work: an initial push
// of every location and tariff, then a heartbeat that keeps ChargeNet fresh. It
// returns at once; the work stops with ctx or with Close.
func (e *Emulator) Run(ctx context.Context) {
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		e.loop(ctx)
	}()
}

// loop pushes once, then re-pushes on the heartbeat until the context ends. A
// heartbeat of zero (tests) pushes nothing, so a test observes only the pushes
// its own commands cause.
func (e *Emulator) loop(ctx context.Context) {
	if e.beat <= 0 {
		return
	}
	e.pushAll(ctx)

	ticker := time.NewTicker(e.beat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.runCtx.Done():
			return
		case <-ticker.C:
			e.pushAll(ctx)
		}
	}
}

// pushAll pushes every location and tariff the emulator owns. ChargeNet upserts them, so re-pushing on the heartbeat is idempotent.
func (e *Emulator) pushAll(ctx context.Context) {
	e.PushLocations(ctx)
	e.PushTariffs(ctx)
}

// PushLocations pushes the emulator's locations to ChargeNet.
// It is exported so the portal's "Simulate CPO registration" button can
// push a freshly registered partner's network.
func (e *Emulator) PushLocations(ctx context.Context) {
	locs, err := e.locationDocs(ctx)
	if err != nil {
		log.Printf("emulator[%s]: reading locations: %v", e.profile.Slug, err)
		return
	}
	for _, loc := range locs {
		path := "/ocpi/emsp/2.3.0/" + ocpi.ModuleLocations + "/" +
			e.profile.CountryCode + "/" + e.profile.PartyID + "/" + loc.ID
		e.push(ctx, "location "+loc.ID, http.MethodPut, path, loc)
	}
}

// PushTariffs pushes the emulator's tariffs to ChargeNet.
func (e *Emulator) PushTariffs(ctx context.Context) {
	for _, t := range e.tariffDocs(ctx) {
		path := "/ocpi/emsp/2.3.0/" + ocpi.ModuleTariffs + "/" +
			e.profile.CountryCode + "/" + e.profile.PartyID + "/" + t.ID
		e.push(ctx, "tariff "+t.ID, http.MethodPut, path, t)
	}
}

// Register performs the spec registration flow against ChargeNet. With the Token A ChargeNet gave it, the emulator reads ChargeNet's
// versions, then its version details, then POSTs its own credentials — its
// token_b and its versions URL — and ChargeNet answers with Token C. Finally it
// pushes its locations and tariffs.
//
// The seed links a demo partner directly, so Register is only needed for a
// partner that is not linked yet: it is what the portal's "Simulate CPO
// registration" button drives for a freshly registered CPO.
func (e *Emulator) Register(ctx context.Context, tokenA string) error {
	tokenA = strings.TrimSpace(tokenA)
	if tokenA == "" {
		return fmt.Errorf("emulator: registration needs a Token A")
	}

	// The two calls a registering CPO makes first.
	if _, _, err := e.registerCall(ctx, http.MethodGet, ocpi.VersionsURLPath, tokenA, nil); err != nil {
		return err
	}
	if _, _, err := e.registerCall(ctx, http.MethodGet, ocpi.VersionURLPath+"/", tokenA, nil); err != nil {
		return err
	}

	// POST credentials: our token_b and our versions URL, as a CPO role.
	creds := ocpi.Credentials{
		Token: e.profile.TokenB,
		URL:   e.SelfURL() + "/versions",
		Roles: []ocpi.CredentialsRole{{
			Role:            ocpi.RoleCPO,
			PartyID:         e.profile.PartyID,
			CountryCode:     e.profile.CountryCode,
			BusinessDetails: ocpi.BusinessDetails{Name: e.profile.Name},
		}},
	}
	status, raw, err := e.registerCall(ctx, http.MethodPost, ocpi.CredentialsPath, tokenA, creds)
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return fmt.Errorf("emulator: registration answered %d: %s", status, strings.TrimSpace(string(raw)))
	}

	// The handshake is done; push the network so ChargeNet has our locations.
	e.PushLocations(ctx)
	e.PushTariffs(ctx)
	return nil
}

// registerCall makes one registration call to ChargeNet with an explicit token
// (Token A, which is what the flow authenticates with — not the operational
// token_c [Emulator.send] presents).
func (e *Emulator) registerCall(ctx context.Context, method, path, token string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(raw)
	}

	cctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, method, e.origin+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Token "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := e.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only response body
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	return resp.StatusCode, raw, nil
}
