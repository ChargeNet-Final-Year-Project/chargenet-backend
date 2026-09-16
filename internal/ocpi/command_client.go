package ocpi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chargenet/chargenet-backend/internal/gateway"
	"github.com/chargenet/chargenet-backend/internal/store"
)

// CommandsPath is the versioned commands module on a CPO's server. It excludes
// ChargeNet's `/ocpi/emsp` receiver prefix.
const CommandsPath = "/" + VersionNumber + "/" + ModuleCommands

// CommandClient sends CPO commands and includes the asynchronous callback URL.
//
// Every command carries a `response_url` (the callback the CPO posts the result
// to) and no more: the synchronous answer comes back on the call itself, which
// [Client] has already unwrapped into a [CommandResponse].
type CommandClient struct {
	client  *Client
	baseURL string
}

// NewCommandClient builds a command sender. publicBaseURL is ChargeNet's
// PUBLIC_BASE_URL: the origin the `response_url` callbacks are built
// from, so a CPO can always reach ChargeNet's receiver.
func NewCommandClient(client *Client, publicBaseURL string) *CommandClient {
	if client == nil {
		client = NewClient()
	}
	return &CommandClient{client: client, baseURL: strings.TrimRight(publicBaseURL, "/")}
}

// ResponseURL is where a CPO posts the asynchronous result of one command: the
// receiver path `{PUBLIC_BASE_URL}/ocpi/emsp/2.2.1/commands/{command}/{uid}`,
// which is exactly the callback [Server.handleCommands] serves.
//
// `uid` is the handle ChargeNet wants the answer keyed by, and the gateway
// chooses it: its own session id for a start or stop, the EVSE uid for an
// unlock. That is what lets the callback be matched back to the thing that is
// waiting without a lookup table.
func (c *CommandClient) ResponseURL(command, uid string) string {
	return c.baseURL + VersionURLPath + "/" + ModuleCommands + "/" + command + "/" + url.PathEscape(uid)
}

// startSessionPayload is the body of `POST /2.2.1/commands/START_SESSION`.
// The token is the driver's because ChargeNet starts every charge on
// behalf of the APP_USER the driver authenticated as, and `auth_method` is
// COMMAND for the same reason.
type startSessionPayload struct {
	ChargeNet              any    `json:"chargenet,omitempty"`
	ResponseURL            string `json:"response_url"`
	Token                  Token  `json:"token"`
	LocationID             string `json:"location_id"`
	EVSEUID                string `json:"evse_uid"`
	ConnectorID            string `json:"connector_id"`
	AuthorizationReference string `json:"authorization_reference"`
}

// stopSessionPayload is the body of `POST /2.2.1/commands/STOP_SESSION`. The CPO
// is named the session by its own id — the one it gave ChargeNet the first time
// it pushed the session (store.LinkOCPISession).
type stopSessionPayload struct {
	ResponseURL string `json:"response_url"`
	SessionID   string `json:"session_id"`
}

// unlockConnectorPayload is the body of `POST /2.2.1/commands/UNLOCK_CONNECTOR`,
// the one command with no session to name.
type unlockConnectorPayload struct {
	ResponseURL string `json:"response_url"`
	LocationID  string `json:"location_id"`
	EVSEUID     string `json:"evse_uid"`
	ConnectorID string `json:"connector_id"`
}

// StartSession asks a CPO to begin the charge a ChargeNet session stands for.
// The driver's token is rendered ChargeNet's way — uid and contract_id are the
// driver's id, type APP_USER, matching the settlement CDR.
func (c *CommandClient) StartSession(ctx context.Context, cpo *store.CpoAccount, target *store.OCPICommandTarget, extensions ...any) (CommandResponse, error) {
	var extension any
	if len(extensions) > 0 {
		extension = extensions[0]
	}
	return c.send(ctx, cpo, CommandStartSession, startSessionPayload{
		ResponseURL:            c.responseURLFor(cpo, CommandStartSession, target.SessionID),
		Token:                  Token{UID: target.UserID, Type: tokenTypeAppUser, ContractID: target.UserID, CountryCode: EMSPCountryCode, PartyID: EMSPPartyID, Issuer: EMSPName, Valid: true, Whitelist: "ALWAYS", LastUpdated: time.Now().UTC().Format(time.RFC3339)},
		LocationID:             target.LocationID,
		EVSEUID:                target.EVSEUID,
		ConnectorID:            target.OCPIConnectorID,
		AuthorizationReference: target.SessionID,
		ChargeNet:              extension,
	})
}

// StopSession asks a CPO to end a charge it is already running. ChargeNet
// answers on the response_url with the same session id, so the gateway knows
// whose stop it is confirming.
func (c *CommandClient) StopSession(ctx context.Context, cpo *store.CpoAccount, target *store.OCPICommandTarget) (CommandResponse, error) {
	return c.send(ctx, cpo, CommandStopSession, stopSessionPayload{
		ResponseURL: c.responseURLFor(cpo, CommandStopSession, target.SessionID),
		SessionID:   target.OCPISessionID,
	})
}

// UnlockConnector releases the cable on a CPO's EVSE. It is keyed by
// the EVSE uid, because there is no session behind it.
func (c *CommandClient) UnlockConnector(ctx context.Context, cpo *store.CpoAccount, target *store.OCPICommandTarget) (CommandResponse, error) {
	return c.send(ctx, cpo, CommandUnlockConnector, unlockConnectorPayload{
		ResponseURL: c.responseURLFor(cpo, CommandUnlockConnector, target.EVSEUID),
		LocationID:  target.LocationID,
		EVSEUID:     target.EVSEUID,
		ConnectorID: target.OCPIConnectorID,
	})
}

// tokenTypeAppUser is the OCPI token type an APP_USER driver's charge is
// started with; see the `cdr_token` of the settlement CDR (internal/sessions).
const tokenTypeAppUser = "APP_USER"

// send posts one command and decodes the synchronous [CommandResponse].
//
// A bare 200 has no command result. Guessing ACCEPTED could report a charge as
// running when it is not, so [Gateway] treats an empty result as failure.
func (c *CommandClient) send(ctx context.Context, cpo *store.CpoAccount, command string, body any) (CommandResponse, error) {
	data, err := c.client.Do(ctx, cpo, http.MethodPost, CommandsPath+"/"+command, body)
	if err != nil {
		return CommandResponse{}, err
	}
	var res CommandResponse
	if len(strings.TrimSpace(string(data))) == 0 {
		return res, nil
	}
	if err := json.Unmarshal(data, &res); err != nil {
		return res, gateway.Errorf(gateway.CodeCPOUnreachable,
			"the CPO's answer to %s could not be read: %v", command, err)
	}
	return res, nil
}

func (c *CommandClient) responseURLFor(cpo *store.CpoAccount, command, id string) string {
	value := c.ResponseURL(command, id)
	if cpo.OCPIVersion == LegacyVersion {
		return strings.Replace(value, "/2.3.0/", "/2.2.1/", 1)
	}
	return value
}
