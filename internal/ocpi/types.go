// Package ocpi implements the eMSP half of OCPI 2.2.1:
// ChargeNet publishes the versions/credentials endpoints a CPO registers
// against, validates the tokens it hands out, and — from OCPI receivers — receives the
// CPO's locations, tariffs, sessions, CDRs and command results.
//
// Only the fields ChargeNet actually uses are modelled, but every one of them
// keeps its spec name (snake_case on the wire) so a real CPO could interoperate
// with the platform without a translation layer.
package ocpi

import "encoding/json"

// OCPI version ChargeNet speaks.
const (
	VersionNumber = "2.3.0"
	// VersionURLPath is the version-details path every endpoint URL is built
	// from.
	VersionURLPath = "/ocpi/emsp/2.3.0"
	// VersionsURLPath is the versions endpoint (Token A is used here during
	// registration).
	VersionsURLPath = "/ocpi/emsp/versions"
	// CredentialsPath is the credentials module on ChargeNet's side.
	CredentialsPath = VersionURLPath + "/credentials"
)

// ChargeNet's own OCPI identity.
const (
	EMSPPartyID     = "CNT"
	EMSPCountryCode = "MY"
	// EMSPName is rendered in the `business_details` block of the credentials
	// ChargeNet replies with.
	EMSPName = "ChargeNet"
)

// Module identifiers used in the version-details endpoint list.
const (
	ModuleCredentials = "credentials"
	ModuleLocations   = "locations"
	ModuleTariffs     = "tariffs"
	ModuleSessions    = "sessions"
	ModuleCDRs        = "cdrs"
	ModuleCommands    = "commands"
)

// Party roles (OCPI `role` enum, the subset we meet).
const (
	RoleCPO  = "CPO"
	RoleEMSP = "EMSP"
)

// Interface roles in the version-details endpoint list. They describe what the
// party publishing the list does in that module: ChargeNet RECEIVEs locations,
// tariffs, sessions, CDRs and command results from its CPOs.
const (
	InterfaceReceiver = "RECEIVER"
	InterfaceSender   = "SENDER"
)

// BusinessDetails is the party's public identity.
type BusinessDetails struct {
	Name    string `json:"name"`
	Website string `json:"website,omitempty"`
}

// CredentialsRole is one role a party registers with (spec: a party may hold
// several roles at the same party_id/country_code).
type CredentialsRole struct {
	Role            string          `json:"role"`
	BusinessDetails BusinessDetails `json:"business_details"`
	PartyID         string          `json:"party_id"`
	CountryCode     string          `json:"country_code"`
}

// Credentials is the credentials module payload, in both directions: the CPO
// sends its own token and versions URL, ChargeNet answers with Token C and its
// versions URL.
type Credentials struct {
	HubPartyID string            `json:"hub_party_id,omitempty"`
	Token      string            `json:"token"`
	URL        string            `json:"url"`
	Roles      []CredentialsRole `json:"roles"`
}

// GeoLocation holds the decimal strings required by OCPI.
type GeoLocation struct {
	Latitude  string `json:"latitude"`
	Longitude string `json:"longitude"`
}

// Version is one entry of the versions endpoint.
type Version struct {
	Version string `json:"version"`
	URL     string `json:"url"`
}

// Endpoint is one entry of the version-details endpoint list.
type Endpoint struct {
	Identifier string `json:"identifier"`
	Role       string `json:"role"`
	URL        string `json:"url"`
}

// VersionDetails is the payload of `GET /ocpi/emsp/2.2.1/`.
type VersionDetails struct {
	Version   string     `json:"version"`
	Endpoints []Endpoint `json:"endpoints"`
}

// EVSE statuses (OCPI `status` enum). OCPI receivers maps the ones the driver app cannot
// represent (RESERVED, OUTOFORDER, …) onto the canonical subset
const (
	EVSEAvailable   = "AVAILABLE"
	EVSECharging    = "CHARGING"
	EVSEBlocked     = "BLOCKED"
	EVSEInoperative = "INOPERATIVE"
	EVSEReserved    = "RESERVED"
	EVSEOutOfOrder  = "OUTOFORDER"
	EVSERemoved     = "REMOVED"
	EVSEUnknown     = "UNKNOWN"
)

// Session statuses a CPO pushes for a charge it runs for us.
const (
	SessionActive    = "ACTIVE"
	SessionCompleted = "COMPLETED"
	SessionPending   = "PENDING"
	SessionInvalid   = "INVALID"
)

// AuthMethod is the only method ChargeNet starts charges with: the driver
// commanded it from the app.
const AuthMethodCommand = "COMMAND"

// Token is an OCPI token reference (drivers are APP_USER tokens here).
type Token struct {
	CountryCode string `json:"country_code"`
	PartyID     string `json:"party_id"`
	Issuer      string `json:"issuer,omitempty"`
	Valid       bool   `json:"valid"`
	Whitelist   string `json:"whitelist,omitempty"`
	LastUpdated string `json:"last_updated,omitempty"`
	UID         string `json:"uid"`
	Type        string `json:"type"`
	ContractID  string `json:"contract_id,omitempty"`
}

// Connector is one connector under a location's EVSE.
type Connector struct {
	ID               string   `json:"id"`
	Standard         string   `json:"standard"`
	Format           string   `json:"format"`
	PowerType        string   `json:"power_type"`
	MaxElectricPower int      `json:"max_electric_power,omitempty"` // watts
	MaxAmperage      float64  `json:"max_amperage,omitempty"`
	MaxVoltage       int      `json:"max_voltage,omitempty"`
	TariffIDs        []string `json:"tariff_ids,omitempty"`
	LastUpdated      string   `json:"last_updated,omitempty"`
}

// EVSE is one charge point of a location.
type EVSE struct {
	Parking                  []map[string]any `json:"parking,omitempty"`
	AcceptedServiceProviders []string         `json:"accepted_service_providers,omitempty"`
	UID                      string           `json:"uid"`
	EVSEID                   string           `json:"evse_id,omitempty"`
	Status                   string           `json:"status"`
	Connectors               []Connector      `json:"connectors"`
	LastUpdated              string           `json:"last_updated,omitempty"`
}

// Location is a CPO's charge location, pushed to ChargeNet.
type Location struct {
	State              string `json:"state,omitempty"`
	Country            string `json:"country"`
	Publish            bool   `json:"publish"`
	TimeZone           string `json:"time_zone"`
	SupportTelephone   string `json:"support_telephone,omitempty"`
	ChargingWhenClosed bool   `json:"charging_when_closed"`

	CountryCode string      `json:"country_code"`
	PartyID     string      `json:"party_id"`
	ID          string      `json:"id"`
	Name        string      `json:"name,omitempty"`
	Address     string      `json:"address"`
	City        string      `json:"city"`
	Coordinates GeoLocation `json:"coordinates"`
	EVSEs       []EVSE      `json:"evse"`
	LastUpdated string      `json:"last_updated,omitempty"`
}

// PriceComponent is one price of a tariff element.
type PriceComponent struct {
	VAT      *float64 `json:"vat,omitempty"`
	Type     string   `json:"type"` // ENERGY | TIME
	Price    float64  `json:"price"`
	StepSize int      `json:"step_size"`
}

// TariffElement groups the price components of one tariff period.
type TariffElement struct {
	Restrictions    json.RawMessage  `json:"restrictions,omitempty"`
	PriceComponents []PriceComponent `json:"price_components"`
}

// Tariff is a simple per-kWh OCPI tariff (the only model in scope).
type Tariff struct {
	RawJSON       json.RawMessage `json:"-"`
	StartDateTime string          `json:"start_date_time,omitempty"`
	EndDateTime   string          `json:"end_date_time,omitempty"`
	MinPrice      json.RawMessage `json:"min_price,omitempty"`
	MaxPrice      json.RawMessage `json:"max_price,omitempty"`

	TaxIncluded        string          `json:"tax_included"`
	PreauthorizeAmount *float64        `json:"preauthorize_amount,omitempty"`
	CountryCode        string          `json:"country_code"`
	PartyID            string          `json:"party_id"`
	ID                 string          `json:"id"`
	Currency           string          `json:"currency"`
	Elements           []TariffElement `json:"elements"`
	LastUpdated        string          `json:"last_updated,omitempty"`
}

// Price is an OCPI price breakdown; ChargeNet stores VAT-inclusive pricing, so
// only `excl_vat` is ever populated.
type Price struct {
	ExclVAT float64     `json:"-"` // internal compatibility name; wire shape is versioned.
	Taxes   []TaxAmount `json:"taxes,omitempty"`
}
type TaxAmount struct {
	Name          string   `json:"name"`
	Amount        float64  `json:"amount"`
	Percentage    *float64 `json:"percentage,omitempty"`
	AccountNumber string   `json:"account_number,omitempty"`
}

func (p Price) MarshalJSON() ([]byte, error) {
	type wire struct {
		BeforeTaxes float64     `json:"before_taxes"`
		Taxes       []TaxAmount `json:"taxes,omitempty"`
	}
	return json.Marshal(wire{p.ExclVAT, p.Taxes})
}
func (p *Price) UnmarshalJSON(raw []byte) error {
	var v struct {
		BeforeTaxes *float64    `json:"before_taxes"`
		ExclVAT     *float64    `json:"excl_vat"`
		Taxes       []TaxAmount `json:"taxes"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	if v.BeforeTaxes != nil {
		p.ExclVAT = *v.BeforeTaxes
	} else if v.ExclVAT != nil {
		p.ExclVAT = *v.ExclVAT
	}
	p.Taxes = v.Taxes
	return nil
}

// Session is what a CPO pushes while it runs a charge for ChargeNet: a
// live snapshot while ACTIVE, the final one when COMPLETED.
type Session struct {
	CountryCode   string  `json:"country_code"`
	PartyID       string  `json:"party_id"`
	ID            string  `json:"id"`
	StartDateTime string  `json:"start_date_time"`
	EndDateTime   string  `json:"end_date_time,omitempty"`
	KWh           float64 `json:"kwh"`
	CDRToken      Token   `json:"cdr_token"`
	AuthMethod    string  `json:"auth_method"`
	// AuthorizationReference is the eMSP reference the CPO was given when the
	// charge started: ChargeNet's own session id, which is how the receiver
	// matches a CPO's session to ours the first time it is pushed.
	AuthorizationReference string `json:"authorization_reference,omitempty"`
	LocationID             string `json:"location_id"`
	EVSEUID                string `json:"evse_uid"`
	ConnectorID            string `json:"connector_id"`
	Currency               string `json:"currency"`
	Status                 string `json:"status"`
	TotalCost              *Price `json:"total_cost,omitempty"`
	// ChargeNet is the non-OCPI block ChargeNet's own CPO emulators add to a
	// Session push, the counterpart of the block of the same name in a
	// CDR: OCPI says how much energy flowed, not how fast or into what state of
	// charge, and the driver's charging screen shows both. A real CPO simply
	// omits it and the screen keeps the values it has.
	ChargeNet   *SessionTelemetry `json:"chargenet,omitempty"`
	LastUpdated string            `json:"last_updated,omitempty"`
}

// SessionTelemetry is the live numbers the ChargeNet block of a Session push
// carries (see [Session.ChargeNet]).
type SessionTelemetry struct {
	CurrentPowerKW  float64 `json:"current_power_kw,omitempty"`
	BatterySoC      float64 `json:"battery_soc,omitempty"`
	DurationSeconds float64 `json:"duration_seconds,omitempty"`
}

// The commands ChargeNet sends a CPO.
const (
	CommandStartSession    = "START_SESSION"
	CommandStopSession     = "STOP_SESSION"
	CommandUnlockConnector = "UNLOCK_CONNECTOR"
)

// Command results. The CPO posts one of these to the
// response_url ChargeNet supplied.
const (
	CommandAccepted     = "ACCEPTED"
	CommandRejected     = "REJECTED"
	CommandNotSupported = "NOT_SUPPORTED"
	CommandTimeout      = "TIMEOUT"
)

// CommandResponse is the synchronous answer to a command, and the body of the
// asynchronous callback.
type CommandResponse struct {
	Result  string `json:"result"`
	Timeout int    `json:"timeout,omitempty"`
	Message string `json:"message,omitempty"`
}

// Dimension is one metered dimension of a charging period.
type Dimension struct {
	Type   string  `json:"type"` // ENERGY | TIME
	Volume float64 `json:"volume"`
}

// ChargingPeriod is one period of the charge the CDR bills.
type ChargingPeriod struct {
	StartDateTime string      `json:"start_date_time"`
	Dimensions    []Dimension `json:"dimensions"`
}

// CDRTariff is the trimmed tariff a CDR embeds.
type CDRTariff struct {
	CountryCode string          `json:"country_code"`
	PartyID     string          `json:"party_id"`
	LastUpdated string          `json:"last_updated"`
	TaxIncluded string          `json:"tax_included"`
	ID          string          `json:"id"`
	Currency    string          `json:"currency"`
	Elements    []TariffElement `json:"elements"`
}

// CDRLocation describes where the charge happened.
type CDRLocation struct {
	Country     string      `json:"country"`
	CountryCode string      `json:"country_code,omitempty"`
	PartyID     string      `json:"party_id,omitempty"`
	ID          string      `json:"id"`
	Name        string      `json:"name,omitempty"`
	Address     string      `json:"address"`
	City        string      `json:"city"`
	Coordinates GeoLocation `json:"coordinates"`
	EVSEUID     string      `json:"evse_uid"`
	// EVSEID is the human-readable charge point id (optional in the spec).
	EVSEID             string `json:"evse_id,omitempty"`
	ConnectorID        string `json:"connector_id"`
	ConnectorStandard  string `json:"connector_standard"`
	ConnectorFormat    string `json:"connector_format"`
	ConnectorPowerType string `json:"connector_power_type"`
}

// ChargeNetRevenue is the platform revenue block added to stored CDRs.
type ChargeNetRevenue struct {
	Gross       float64 `json:"gross"`
	PlatformFee float64 `json:"platform_fee"`
	NetPayout   float64 `json:"net_payout"`
}

// CDR is the charge detail record stored in `cdrs.cdr_json`.
// [internal/sessions.BuildCDR] writes it; defining the document here as types is
// what lets the receiver and portal read one back without handwritten maps. A
// test decodes a built document into this struct to keep the
// two in step.
type CDR struct {
	CountryCode     string           `json:"country_code"`
	PartyID         string           `json:"party_id"`
	ID              string           `json:"id"`
	StartDateTime   string           `json:"start_date_time"`
	EndDateTime     string           `json:"end_date_time"`
	SessionID       string           `json:"session_id"`
	CDRToken        Token            `json:"cdr_token"`
	AuthMethod      string           `json:"auth_method"`
	CDRLocation     CDRLocation      `json:"cdr_location"`
	Currency        string           `json:"currency"`
	Tariffs         []CDRTariff      `json:"tariffs"`
	ChargingPeriods []ChargingPeriod `json:"charging_periods"`
	TotalCost       Price            `json:"total_cost"`
	TotalEnergy     float64          `json:"total_energy"`
	TotalTime       float64          `json:"total_time"`
	LastUpdated     string           `json:"last_updated"`
	ChargeNet       ChargeNetRevenue `json:"chargenet"`
}

func (t *Tariff) UnmarshalJSON(raw []byte) error {
	type alias Tariff
	var value alias
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*t = Tariff(value)
	t.RawJSON = append([]byte(nil), raw...)
	return nil
}
