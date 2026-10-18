# Simulated regional network

The additive seed migration adds 24 stations and 48 connectors. Existing Malaysian demonstration stations are retained. Operator identities, addresses and flags identify simulated sites; these are not claims of commercial operator or property affiliation. Stable IDs and one-time migrations preserve existing records and prevent duplicate seeds.

Country is a station attribute. Every emulator remains registered as a Malaysian OCPI party and operates sites in MY, SG and TH; tariff currency comes from its tariff, not its party registration.

| Area | Country | Emulator | Latitude | Longitude |
| --- | --- | --- | ---: | ---: |
| George Town | MY | voltgo | 5.419000 | 100.331000 |
| Gurney Bay | MY | chargesync | 5.437200 | 100.310000 |
| Tanjung Tokong | MY | ecowatt | 5.459000 | 100.307000 |
| Air Itam | MY | chargesync | 5.403300 | 100.278000 |
| Bayan Lepas / Queensbay area | MY | voltgo | 5.334000 | 100.305000 |
| Batu Ferringhi | MY | ecowatt | 5.473000 | 100.250000 |
| Butterworth | MY | voltgo | 5.399000 | 100.368000 |
| Seberang Jaya | MY | chargesync | 5.394000 | 100.399000 |
| Bukit Mertajam | MY | ecowatt | 5.363000 | 100.461000 |
| Juru / Auto-City area | MY | chargesync | 5.338000 | 100.427000 |
| Batu Kawan | MY | voltgo | 5.245000 | 100.438000 |
| Marina Bay | SG | chargesync | 1.280000 | 103.854000 |
| Jurong East | SG | voltgo | 1.334000 | 103.742000 |
| Tampines | SG | ecowatt | 1.353000 | 103.945000 |
| Woodlands | SG | voltgo | 1.437000 | 103.786000 |
| Orchard Road | SG | ecowatt | 1.304000 | 103.832000 |
| Hat Yai | TH | voltgo | 7.005000 | 100.471000 |
| Songkhla | TH | ecowatt | 7.197000 | 100.592000 |
| Phuket Town | TH | chargesync | 7.885000 | 98.389000 |
| Bangkok | TH | chargesync | 13.733000 | 100.535000 |
| Hat Yai East | TH | ecowatt | 7.008000 | 100.500000 |
| Songkhla South | TH | chargesync | 7.170000 | 100.607000 |
| Marina South | SG | voltgo | 1.274000 | 103.866000 |
| Bangkok West | TH | voltgo | 13.724000 | 100.510000 |

Penang Island: George Town, Gurney Bay, Tanjung Tokong, Air Itam, Bayan Lepas/Queensbay area and Batu Ferringhi. Mainland: Butterworth, Seberang Jaya, Bukit Mertajam, Juru/Auto-City area and Batu Kawan. The Singapore and Thailand rows cover the requested areas and additional operator distribution.

Each location has a 22 kW Type 2 AC connector and a CCS2 DC connector (VoltGo 150 kW, others 60 kW). The original network retains its other power tiers and standards. Batu Ferringhi DC is INOPERATIVE and one Bangkok AC connector is BLOCKED, providing distinct demonstration states.

| Tariff currency | AC / kWh | VoltGo and ChargeSync DC / kWh | EcoWatt DC / kWh |
| --- | ---: | ---: | ---: |
| MYR | 0.90 | 1.50 | 1.10 |
| SGD | 0.45 | 0.60 | 0.55 |
| THB | 6.50 | 8.00 | 7.50 |

All demo tariffs declare that no taxes apply. They are simulated prices and do not represent market prices or real charging-site offers. The naming convention is `stn_demo_<area>`, `con_demo_<area>_<ac|dc>` and `trf_demo_<operator>_<country>_<ac|dc>`. EVSE IDs include physical country, registered operator party and a stable demo suffix, for example `SG*CSY*E*D12DC`.

Geographical references used to check the neighbourhood distribution: [official Penang map](https://mypenang.gov.my/uploads/downloads/PTM-ENG2026-1.pdf), [Penang Port ferry connection between George Town and Butterworth](https://penangport.gov.my/en/services/service/ferry-services), [Singapore NParks Woodlands geography](https://www.nparks.gov.sg/visit/parks/park-detail/woodlands-waterfront-park), and [Tourism Authority of Thailand: Hat Yai](https://www.tourismthailand.org/Destinations/Provinces/Hat-Yai/362). These support the regional context; the exact fictional site coordinates and street descriptions are plausible demo placements, not verified charger addresses.


State and region discovery is stored separately from city and country. Migration
006 labels Penang; Singapore Central/East/North/West; Thai Bangkok/Phuket/Songkhla;
and the known Malaysian legacy areas. `GET /api/v1/stations/regions` returns actual
active coverage and geographic bounds. OCPI location `state` is retained as the
region; a push without it preserves an existing region.

Station names follow operator/location conventions, for example VoltGo George
Town and ChargeSync Marina Bay. Migration 007 renames only exact old seed names
at known seed IDs and expected operators; custom names and financial records survive.
