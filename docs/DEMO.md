# Reproducible demonstration

Start PostgreSQL and the API as described in the README. To run the scripted
MYR-wallet charging journey against the local simulation:

```sh
python scripts/demo_roaming.py --base http://localhost:8080 --output demo-roaming-results.json
```

This uses the public demo account, may add simulated wallet funds, and starts and
stops real emulator sessions in the selected development database. It creates
financial demonstration records. It does not reset the database or contact a
commercial charging or payment service. For repeatable tests without changing
demonstration history, use the isolated backend integration suite instead.

For a visual demonstration, start the mobile and portal companion applications:

1. Sign in as the demo driver. Search/filter MY, SG and TH; select a connector.
2. Inspect the operator, availability, original tariff and available wallet funds.
3. Select a vehicle/target, review the estimated total, then confirm and start.
4. Monitor live cost, battery and energy. Interrupted updates remain visibly
   uncertain and can reconnect; minimising does not stop charging.
5. Stop, inspect the settled receipt, wallet ledger and charging history.
6. In the CPO portal, inspect that operator's network and original-currency CDR.
   Currency totals are separate; sandbox charges do not appear as operator revenue.
7. Register a new CPO, configure/verify credentials, then approve it using an
   administrator account. Verification alone does not approve marketplace access.

The integration tests additionally cover nine operator/country combinations,
currency combinations, changed pricing, command failures, repeated requests,
interrupted telemetry/recovery and duplicate settlement protection.
