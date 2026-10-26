# ChargeNet backend

Go/PostgreSQL services for a simulated EV-charging network. One driver account
can use three simulated CPOs across MY, SG and TH. The companion applications are
`chargenet-mobile` and `chargenet-portal`; neither is needed to run backend tests.

## Requirements

- Go 1.23 or newer compatible with `go.mod`.
- Docker with Compose for PostgreSQL 16.
- Python 3 only for the optional demonstration script.

## Run locally

```sh
docker compose up -d
# Optional: copy .env.example to .env and adjust the values.
go run ./cmd/server
```

The API listens on `http://localhost:8080`; `GET /healthz` checks the database.
Startup applies the embedded bootstrap and ordered migrations transactionally,
then starts session recovery, the OCPI emulators and periodic synchronisation.
Existing databases are upgraded without a reset. `docker compose down -v` deletes
the database volume and is not required for upgrades.

## Demonstration accounts

| Role | Login | Initial password |
| --- | --- | --- |
| Driver | demo@chargenet.my | Demo1234! |
| Administrator | admin@chargenet.my | Admin1234! |
| CPO operator | ops@voltgo.my / ops@chargesync.my / ops@ecowatt.my | Cpo1234! |

These are public development fixtures, not production credentials. The initial
driver wallet contains MYR 85.50; existing balances are never reset on startup.

## Configuration

| Variable | Default / purpose |
| --- | --- |
| PORT | 8080 |
| DATABASE_URL | Local Compose PostgreSQL; see .env.example |
| JWT_SECRET | Development signing key; change outside a local demonstration |
| PUBLIC_BASE_URL | http://localhost:8080; callback origin |
| PORTAL_ORIGIN | http://localhost:5173; cookie-enabled portal CORS origin |
| GATEWAY | ocpi; `local` selects the in-process simulator |
| SIM_TIME_SCALE | 10 simulated seconds per real second |
| PLATFORM_FEE_PCT | 5; operator revenue share retained by ChargeNet |
| MIN_START_BALANCE | 10 wallet-currency units |
| FX_RATE_SOURCE | Optional simulated-rate source filter; empty selects the latest available source |

Estimates do not expire. Start revalidates the tariff and latest effective FX;
changed financial terms require the driver to review and confirm again.

## Verify

```sh
go test -count=1 -p 1 ./...
go vet ./...
go build ./...
```

Integration tests create/use `chargenet_test`, never the normal application
database. Keep `-p 1`: packages share test fixtures. Tests skip database scenarios
when PostgreSQL is unavailable, so inspect the output before claiming integration
success. Override `TEST_DATABASE_URL` only with a dedicated test database.

Use `go test -race -count=1 -p 1 ./...` on a host with CGO and a compatible C
compiler. Standard tests work on Windows without CGO. CI enables the race detector.

## Structure and documentation

- `cmd/server`: startup and shutdown.
- `internal/api`: driver and tenant/role-scoped portal HTTP APIs.
- `internal/store`: database access, migrations and transactional accounting.
- `internal/sessions`, `gateway`, `simulation`: lifecycle and charging simulation.
- `internal/ocpi`: protocol subset, synchronisation and three CPO emulators.
- `scripts/migrations`: additive upgrades; `scripts/demo_roaming.py`: HTTP demo.
- [Architecture](docs/ARCHITECTURE.md), [API](docs/API.md), [demo](docs/DEMO.md),
  [regional network](docs/REGIONAL_NETWORK.md), and [testing](docs/TESTING.md).

## Development

ChargeNet uses rapid prototyping. Each increment starts on a short branch from
`main`, uses focused commits, and is merged with a merge commit after the relevant
checks pass. Shared increments use the same branch name in each repository they
change. Repositories unaffected by an increment receive no branch or commit.
