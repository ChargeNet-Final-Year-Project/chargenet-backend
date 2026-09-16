package ocpi_test

// Gateway integration tests cover commands sent to a fake CPO and callbacks
// received through the real receiver. The fake CPO serves a partner's
// `/2.3.0/commands` endpoints, and the real receiver, served by httptest, is
// where that CPO posts its asynchronous answers.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path"
	"sync"
	"testing"
	"time"

	"github.com/chargenet/chargenet-backend/internal/config"
	"github.com/chargenet/chargenet-backend/internal/gateway"
	"github.com/chargenet/chargenet-backend/internal/ocpi"
	"github.com/chargenet/chargenet-backend/internal/simulation"
	"github.com/chargenet/chargenet-backend/internal/store"
	"github.com/chargenet/chargenet-backend/internal/testutil"
)

// cpoCall is one command ChargeNet sent, as the fake CPO saw it.
type cpoCall struct {
	Method string
	Path   string
	Token  string
	Body   map[string]any
}

// fakeCPO is a CPO partner's commands endpoint in miniature. It records every
// call and answers the result a test asks for, so the outbound half
// can be asserted without a CPO emulator (internal/ocpi/emulator is not built).
type fakeCPO struct {
	mu     sync.Mutex
	calls  []cpoCall
	result string
	empty  bool
	after  func(command string, body map[string]any)
}

func newFakeCPO(t *testing.T) (*fakeCPO, *httptest.Server) {
	t.Helper()
	f := &fakeCPO{result: ocpi.CommandAccepted}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeCPO) serve(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{}
	_ = json.NewDecoder(r.Body).Decode(&body)
	command := path.Base(r.URL.Path)

	f.mu.Lock()
	f.calls = append(f.calls, cpoCall{Method: r.Method, Path: r.URL.Path, Token: r.Header.Get("Authorization"), Body: body})
	result, empty, after := f.result, f.empty, f.after
	f.mu.Unlock()

	if empty {
		w.WriteHeader(http.StatusOK)
	} else {
		ocpi.OK(w, http.StatusOK, ocpi.CommandResponse{Result: result})
	}
	if after != nil {
		after(command, body)
	}
}

// answer sets the synchronous result the CPO replies with.
func (f *fakeCPO) answer(result string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.result, f.empty = result, false
}

// blank makes the CPO answer a bare 200 with no body at all.
func (f *fakeCPO) blank() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.empty = true
}

// onAnswer installs what the CPO does once its synchronous answer is written —
// where a test makes it post its asynchronous result back to the response_url.
func (f *fakeCPO) onAnswer(fn func(command string, body map[string]any)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.after = fn
}

// callsMade returns a copy of every call the CPO has seen.
func (f *fakeCPO) callsMade() []cpoCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]cpoCall(nil), f.calls...)
}

// awaitCall waits for the CPO to have been called n times, so a test does not
// race the goroutine that is sending the command.
func (f *fakeCPO) awaitCall(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(f.callsMade()) >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for call %d; saw %+v", n, f.callsMade())
}

// onlyCall fails unless the CPO was called exactly once, and returns that call.
func (f *fakeCPO) onlyCall(t *testing.T) cpoCall {
	t.Helper()
	calls := f.callsMade()
	if len(calls) != 1 {
		t.Fatalf("the CPO was called %d times, want 1: %+v", len(calls), calls)
	}
	return calls[0]
}

// gatewayFixture is the three pieces the outbound path joins: the session to
// command, the real OCPI gateway, and the receiver the CPO posts back to.
type gatewayFixture struct {
	Gateway  *ocpi.Gateway
	Store    *store.Store
	Charge   chargeFixture
	Receiver *httptest.Server
	Fake     *fakeCPO
}

// newGatewayFixture wires the real gateway against the shared test database, a
// real receiver served by httptest (so the response_url callbacks the gateway
// builds are reachable) and a fake CPO the partner row is pointed at.
func newGatewayFixture(t *testing.T, partyID string) *gatewayFixture {
	t.Helper()
	fake, cpoSrv := newFakeCPO(t)
	st := testutil.NewTestStore(t)
	c := newFixtureCharge(t, st, partyID)

	// The outbound half authenticates with Token B and calls the partner's base
	// URL; the fixture partner has neither, so this is where it gets them.
	if _, err := st.DB.ExecContext(context.Background(),
		`UPDATE cpo_partners SET base_url = $1, token_b = $2, ocpi_version = $3 WHERE id = $4`,
		cpoSrv.URL, partyID+"-token-b", ocpi.VersionNumber, c.CPOID); err != nil {
		t.Fatalf("point the fixture cpo at the fake: %v", err)
	}

	srv := ocpi.New(config.Config{PublicBaseURL: publicBaseURL, PlatformFeePct: 5}, st)
	receiver := httptest.NewServer(srv.Handler())
	t.Cleanup(receiver.Close)

	gw := ocpi.NewGateway(st, config.Config{PublicBaseURL: publicBaseURL}, ocpi.WithBaseURL(receiver.URL))
	t.Cleanup(gw.Close)

	// The gateway is both the orchestrator's sink and the commands receiver's
	// sink — one object, the two seams of OCPI receivers that meet here.
	srv.Sink, srv.Commands = gw, gw

	return &gatewayFixture{Gateway: gw, Store: st, Charge: c, Receiver: receiver, Fake: fake}
}

// linkOCPISession gives the fixture session the CPO's own name for it, which is
// what a STOP_SESSION names it by.
func (f *gatewayFixture) linkOCPISession(t *testing.T, ocpiSessionID string) {
	t.Helper()
	if err := f.Store.LinkOCPISession(context.Background(), f.Charge.CPOID, f.Charge.SessionID, ocpiSessionID); err != nil {
		t.Fatalf("link the ocpi session: %v", err)
	}
}

// assertCode fails unless err carries the given gateway code; want "" means no
// error at all.
func assertCode(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("want the code %s, got no error", want)
	}
	if got := gateway.CodeOf(err); got != want {
		t.Fatalf("error code = %q (%v), want %q", got, err, want)
	}
}

// postCommandResult exercises the same callback path used by a CPO.
func postCommandResult(t *testing.T, responseURL, tokenC string, res ocpi.CommandResponse) {
	t.Helper()
	body, err := json.Marshal(res)
	if err != nil {
		t.Errorf("encode the command result: %v", err)
		return
	}
	req, err := http.NewRequest(http.MethodPost, responseURL, bytes.NewReader(body))
	if err != nil {
		t.Errorf("build the command result request: %v", err)
		return
	}
	req.Header.Set("Authorization", "Token "+tokenC)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Errorf("post the command result: %v", err)
		return
	}
	defer resp.Body.Close() //nolint:errcheck // read-only response body
	if resp.StatusCode != http.StatusOK {
		t.Errorf("the receiver answered the command result with %d", resp.StatusCode)
	}
}

// TestGatewayStartSessionSendsThePlanCommand covers OCPI receivers / 2.9: the
// START_SESSION ChargeNet sends a CPO carries the driver's token, the location,
// EVSE and connector, and a response_url the CPO posts its answer back to.
func TestGatewayStartSessionSendsThePlanCommand(t *testing.T) {
	f := newGatewayFixture(t, "T60")
	f.Fake.answer(ocpi.CommandAccepted)

	if err := f.Gateway.StartSession(context.Background(), gateway.StartCmd{SessionID: f.Charge.SessionID}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	call := f.Fake.onlyCall(t)
	if call.Method != http.MethodPost {
		t.Fatalf("method = %s, want POST", call.Method)
	}
	if want := ocpi.CommandsPath + "/" + ocpi.CommandStartSession; call.Path != want {
		t.Fatalf("path = %s, want %s", call.Path, want)
	}
	if want := "Token " + base64.StdEncoding.EncodeToString([]byte("T60-token-b")); call.Token != want {
		t.Fatalf("Authorization = %q, want %q", call.Token, want)
	}

	wantURL := f.Receiver.URL + ocpi.VersionURLPath + "/commands/" + ocpi.CommandStartSession + "/" + f.Charge.SessionID
	if call.Body["response_url"] != wantURL {
		t.Fatalf("response_url = %v, want %s", call.Body["response_url"], wantURL)
	}
	if call.Body["location_id"] != "T60-LOC" {
		t.Fatalf("location_id = %v, want T60-LOC", call.Body["location_id"])
	}
	if call.Body["evse_uid"] != f.Charge.EVSEUID {
		t.Fatalf("evse_uid = %v, want %s", call.Body["evse_uid"], f.Charge.EVSEUID)
	}
	if call.Body["connector_id"] != "1" {
		t.Fatalf("connector_id = %v, want 1", call.Body["connector_id"])
	}
	if call.Body["authorization_reference"] != f.Charge.SessionID {
		t.Fatalf("authorization_reference = %v, want %s", call.Body["authorization_reference"], f.Charge.SessionID)
	}
	token, ok := call.Body["token"].(map[string]any)
	if !ok {
		t.Fatalf("the start carried no token: %v", call.Body)
	}
	if token["uid"] != f.Charge.UserID || token["contract_id"] != f.Charge.UserID || token["type"] != "APP_USER" {
		t.Fatalf("start token = %+v, want the driver %s as APP_USER", token, f.Charge.UserID)
	}
}

// TestGatewayStartSessionReportsTheCPOsAnswer covers the design: a CPO that turns
// the start down is a connector problem, and anything else means the partner
// could not be used at all — never a charge that is silently reported running.
func TestGatewayStartSessionReportsTheCPOsAnswer(t *testing.T) {
	f := newGatewayFixture(t, "T61")
	cases := []struct {
		name   string
		answer func()
		want   string
	}{
		{"REJECTED is the connector", func() { f.Fake.answer(ocpi.CommandRejected) }, gateway.CodeConnectorUnavailable},
		{"NOT_SUPPORTED is the partner", func() { f.Fake.answer(ocpi.CommandNotSupported) }, gateway.CodeCPOUnreachable},
		{"TIMEOUT is the partner", func() { f.Fake.answer(ocpi.CommandTimeout) }, gateway.CodeCPOUnreachable},
		{"an unknown result is the partner", func() { f.Fake.answer("WAT") }, gateway.CodeCPOUnreachable},
		{"an empty body is the partner", func() { f.Fake.blank() }, gateway.CodeCPOUnreachable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.answer()
			err := f.Gateway.StartSession(context.Background(), gateway.StartCmd{SessionID: f.Charge.SessionID})
			assertCode(t, err, tc.want)
		})
	}
}

// TestGatewayStopSessionPostsBackThroughTheReceiver covers the whole outbound
// loop: StopSession sends STOP_SESSION, and the CPO's asynchronous answer
// travels back through the real receiver and wakes the caller. Nothing is
// stubbed between the halves — the fake CPO really POSTs to the response_url.
func TestGatewayStopSessionPostsBackThroughTheReceiver(t *testing.T) {
	f := newGatewayFixture(t, "T62")
	f.linkOCPISession(t, "T62-CS")
	f.Fake.answer(ocpi.CommandAccepted)
	f.Fake.onAnswer(func(command string, body map[string]any) {
		if command != ocpi.CommandStopSession {
			return
		}
		url, _ := body["response_url"].(string)
		go postCommandResult(t, url, "T62-token-c", ocpi.CommandResponse{Result: ocpi.CommandAccepted})
	})

	done := make(chan error, 1)
	go func() { done <- f.Gateway.StopSession(context.Background(), f.Charge.SessionID) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("StopSession: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("StopSession never returned; the result callback did not wake it")
	}

	call := f.Fake.onlyCall(t)
	if want := ocpi.CommandsPath + "/" + ocpi.CommandStopSession; call.Path != want {
		t.Fatalf("path = %s, want %s", call.Path, want)
	}
	if call.Body["session_id"] != "T62-CS" {
		t.Fatalf("session_id = %v, want the CPO's own name T62-CS", call.Body["session_id"])
	}
	if call.Body["response_url"] != f.Receiver.URL+ocpi.VersionURLPath+"/commands/"+ocpi.CommandStopSession+"/"+f.Charge.SessionID {
		t.Fatalf("response_url = %v, want a URL keyed by ChargeNet's session id", call.Body["response_url"])
	}
}

// TestGatewayStopSessionEndsOnTheSessionsTerminalEvent covers the other half of
// OCPI receivers: a CPO that confirms the stop only by pushing the session's
// final state must end the wait just as a command result does.
func TestGatewayStopSessionEndsOnTheSessionsTerminalEvent(t *testing.T) {
	f := newGatewayFixture(t, "T63")
	f.linkOCPISession(t, "T63-CS")
	f.Fake.answer(ocpi.CommandAccepted)

	done := make(chan error, 1)
	go func() { done <- f.Gateway.StopSession(context.Background(), f.Charge.SessionID) }()
	f.Fake.awaitCall(t, 1)

	f.Gateway.Publish(gateway.Telemetry{
		SessionID: f.Charge.SessionID,
		Status:    gateway.StatusCompleted,
		Reason:    simulation.ReasonRemoteStopped,
	})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("StopSession: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the session's terminal event did not end the stop")
	}
}

// TestGatewayStopSessionGivesUpWhenTheCPONeverAnswers covers the timeout: a stop
// the CPO never confirms must not hang a driver, so the context ends the wait
// with CPO_UNREACHABLE.
func TestGatewayStopSessionGivesUpWhenTheCPONeverAnswers(t *testing.T) {
	f := newGatewayFixture(t, "T64")
	f.linkOCPISession(t, "T64-CS")
	f.Fake.answer(ocpi.CommandAccepted)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	assertCode(t, f.Gateway.StopSession(ctx, f.Charge.SessionID), gateway.CodeCPOUnreachable)
}

// TestGatewayAsyncStartRejectionFailsTheSession covers OCPI receivers: a CPO
// that rejects an accepted start publishes FAILED so the orchestrator can settle
// the session and release the driver.
func TestGatewayAsyncStartRejectionFailsTheSession(t *testing.T) {
	f := newGatewayFixture(t, "T65")
	sub := f.Gateway.Subscribe()

	f.Gateway.CommandResult(ocpi.CommandStartSession, f.Charge.SessionID, ocpi.CommandResponse{
		Result:  ocpi.CommandRejected,
		Message: "the bay is blocked",
	})

	select {
	case ev := <-sub:
		if ev.SessionID != f.Charge.SessionID || ev.Status != gateway.StatusFailed || !ev.Terminal() {
			t.Fatalf("event = %+v, want a terminal FAILED for %s", ev, f.Charge.SessionID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the rejected start published no terminal event")
	}
}

// TestGatewayUnlockConnector covers the design UNLOCK_CONNECTOR: it is addressed by
// the EVSE uid, a CPO that refuses it is a connector problem, and an EVSE
// ChargeNet does not know is one too — never a call to the wrong partner.
func TestGatewayUnlockConnector(t *testing.T) {
	f := newGatewayFixture(t, "T66")

	f.Fake.answer(ocpi.CommandAccepted)
	if err := f.Gateway.UnlockConnector(context.Background(), f.Charge.EVSEUID); err != nil {
		t.Fatalf("UnlockConnector: %v", err)
	}
	call := f.Fake.onlyCall(t)
	if want := ocpi.CommandsPath + "/" + ocpi.CommandUnlockConnector; call.Path != want {
		t.Fatalf("path = %s, want %s", call.Path, want)
	}
	if call.Body["evse_uid"] != f.Charge.EVSEUID {
		t.Fatalf("evse_uid = %v, want %s", call.Body["evse_uid"], f.Charge.EVSEUID)
	}

	f.Fake.answer(ocpi.CommandRejected)
	assertCode(t, f.Gateway.UnlockConnector(context.Background(), f.Charge.EVSEUID), gateway.CodeConnectorUnavailable)
	assertCode(t, f.Gateway.UnlockConnector(context.Background(), "MY*NOBODY*E9"), gateway.CodeConnectorUnavailable)
}

// TestGatewayReportsWhatItCannotAddress covers the addressing failures: a
// session ChargeNet does not have, a stop before the CPO has named the session,
// and a call with no identifier at all are each turned away before anything is
// sent.
func TestGatewayReportsWhatItCannotAddress(t *testing.T) {
	f := newGatewayFixture(t, "T67")

	assertCode(t, f.Gateway.StartSession(context.Background(), gateway.StartCmd{SessionID: "csn_nobody"}), gateway.CodeSessionNotFound)
	assertCode(t, f.Gateway.StartSession(context.Background(), gateway.StartCmd{}), gateway.CodeValidation)
	assertCode(t, f.Gateway.StopSession(context.Background(), f.Charge.SessionID), gateway.CodeSessionNotFound)
	assertCode(t, f.Gateway.UnlockConnector(context.Background(), ""), gateway.CodeValidation)

	if calls := f.Fake.callsMade(); len(calls) != 0 {
		t.Fatalf("a command that should have been refused reached the CPO: %+v", calls)
	}
}
