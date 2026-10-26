# HTTP API

The backend exposes three authentication contexts. Driver JSON uses camelCase;
OCPI objects use protocol snake_case. Timestamps use RFC 3339. Estimate prices,
rates and totals are decimal strings; settled money has two decimal places.

## Driver routes

| Route | Purpose |
| --- | --- |
| POST /api/v1/auth/register | Driver account; optional walletCurrency MYR/SGD/THB |
| POST /api/v1/auth/login; GET /api/v1/auth/me | Sign in and restore identity |
| GET /api/v1/stations; GET /api/v1/stations/regions | Discovery and actual administrative-region coverage |
| GET /api/v1/stations/{id}; GET /api/v1/evse/{uid} | Station or selected connector details |
| /api/v1/vehicles | Driver-owned vehicle CRUD/default selection |
| GET /api/v1/wallet | Balance, reserved and available funds |
| POST /api/v1/wallet/topup; GET /api/v1/wallet/transactions | Simulated top-up and ledger |
| POST /api/v1/quotes; GET /api/v1/quotes/{id} | Create/read a nonexpiring owner-scoped estimate |
| POST /api/v1/sessions/start | Validate accepted pricing and request remote activation |
| GET /api/v1/sessions/active; GET /api/v1/sessions/history | Active charge and history |
| GET /api/v1/sessions/{id}; GET /api/v1/sessions/{id}/stream | Snapshot and SSE telemetry |
| POST /api/v1/sessions/{id}/stop | Stop and settle; repeated stops retain the committed result |

Authenticated driver calls use `Authorization: Bearer <token>`. Send a stable
`Idempotency-Key` for retries of the same start or top-up operation. Successful
replays set `Idempotent-Replay: true`.

Discovery supports `query`, `country`, `region`, `powerType`, `minPower`, `maxPower`,
`connectorType`, `cpoId`, `availableOnly`, and optional location/radius parameters.
Power/type/availability conditions apply to the same matching connector.

```json
POST /api/v1/quotes
{"evseUid":"SG*CSY*E*D12DC","estimatedKwh":"15"}
```

The estimate response includes its ID, tariff version/price, original and wallet
currencies, FX record/rate/source, estimated amounts, energy and spending limit.
It has no expiresAt field. The API retains the internal name `quote`; the app
calls it an estimated total.

```json
POST /api/v1/sessions/start
{"evseUid":"SG*CSY*E*D12DC","quoteId":"qte_...","targetSoc":80}
```

Start may also include vehicleId. `QUOTE_CHANGED` (409) means current pricing
differs: obtain an updated estimate and require confirmation again. A newer FX
record with the same effective value needs no extra interaction. The returned
session retains accepted pricing, original cost and wallet cost. CDRs retain
operator currency; wallet transactions retain wallet currency.

Errors use `{"error":{"code":"CODE","message":"Description"}}`, with optional
field/required/balance information. Other relevant codes include
CONNECTOR_UNAVAILABLE, INSUFFICIENT_BALANCE, INCOMPATIBLE_CONNECTOR,
SESSION_ALREADY_ACTIVE, QUOTE_REQUIRED, QUOTE_CONSUMED, RATE_UNAVAILABLE,
TARIFF_UNSUPPORTED and CPO_UNREACHABLE. There is no quotation-expired workflow.

## Portal routes

`POST /admin/api/register`, `/login`, `/logout` and `GET /admin/api/me` manage the
HttpOnly `cn_portal` cookie. CPO routes cover company profile, token-A regeneration,
connection verification/disconnection, `/network`, `/overview`, `/cdrs` and
`/sandbox/command`. `/admin/api/admin/*` requires SUPER_ADMIN. Tenant CDR details
and CSV exports are scoped by CPO, dates and EVSE; aggregates select one currency.
Business reporting days use Asia/Kuala_Lumpur. Sandbox charges are excluded from
operator revenue. CSV `format=csv` exports the complete matching range.

## OCPI routes

`/ocpi/emsp/versions` advertises the implemented version endpoints. Integration
calls use `Authorization: Token <token>`. Token A registers credentials once;
tokens B/C authenticate the two operational directions. Verification and admin
approval are separate. Pending partners cannot use operational Token C.

The emulators expose `/emulator/{voltgo,chargesync,ecowatt}/ocpi`. This is a
controlled development network, not a commercial OCPI certification claim.
