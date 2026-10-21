# Verification

Run `go test -count=1 -p 1 ./...` with PostgreSQL available. The test helper uses
an isolated database named `chargenet_test` by default and refuses destructive
setup outside its test-name convention. SQL integration suites truncate their
own mutable fixtures; never point them at demonstration/production data.

Coverage includes owner/tenant/role boundaries, CPO credential registration,
location/tariff push and pull, unavailable connectors, incompatible vehicles,
insufficient funds, source currency preservation, nine wallet/tariff currency
combinations, unchanged/changed/latest FX, nonexpiring historical estimates,
concurrent starts/debits, locked settlement, failed commands and restart recovery.

`TestRoamingAcrossEveryEmulatorAndCountry` exercises one driver across three
operators and three countries using driver REST handlers and real loopback OCPI
HTTP. It verifies telemetry, repeated-start replay, stop, wallet debit, CDR and
duplicate-stop protection. Widget/device behavior is tested in the mobile repo.

`go vet ./...` and `go build ./...` check the service and tools. Use the race
detector on a CGO-capable machine. CI uses PostgreSQL and `-race`; local skipped
tests or an unavailable race detector must not be described as executed passes.
