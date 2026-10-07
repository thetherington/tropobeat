# Tropobeat Rolling Plan

**Automated Tropospheric Ducting & Telemetry Correlation Engine**

| | |
|---|---|
| Last updated | 2026-10-07 |
| Current phase | Phase 1: Weather data (Open-Meteo) |
| Platform | Evertz Magnum Analytics (inSITE), Elasticsearch, Open-Meteo API |

This is a living document. Update it at the end of each work session: move finished items to **Done**, add new findings to **Open questions** and **Decisions**, and add a line to the **Change log**. Question numbers are never reused, so gaps mean a question was resolved (see **Decisions**).

Status legend: ✅ done · 🔄 in progress · ⏳ next up · 📋 planned · ❓ blocked on a question

---

## 1. Goal

VHF/UHF TV signals (54–608 MHz) normally stop at the radio horizon. Tropospheric ducts can carry them hundreds of miles further. When a distant station arrives at a Local Receive Facility (LRF) on the same RF channel as the local station it receives, the result is Co-Channel Interference (CCI). MER collapses, and the local channel pixelates or drops to "No Signal".

Tropobeat will run inside inSITE and:

1. **Detect** active ducting events by correlating atmospheric data from the Open-Meteo API with real-time MER, Pre-FEC BER and RF power at all 211 LRFs.
2. **Explain** each degradation by naming the likely interfering co-channel transmitter and propagation path.
3. **Predict** at-risk LRFs before an event, using forecast ducting conditions.

The aim is to move operations from reactive troubleshooting to proactive, data-driven root-cause analysis.

## 2. Architecture (target)

```
 LRF reference data ──┐
 FCC licensing data ──┴──► reference indices (Elasticsearch) ──► coordinates (LRFs, path points, transmitters)
                                                                        │
 Open-Meteo API ◄── batched queries ── beater/openmeteo ◄───────────────┘
                                             │ profiles (surface + pressure levels)
                                             ▼
                                       beater/ducting ──► weather / ducting indices ──┐
                                                                                     ├──► correlation ──► events index ──► inSITE dashboards,
 RF telemetry (MER, Pre-FEC BER, power) ──► existing inSITE telemetry indices ──────┘      (beat)                         maps & alerts
```

- **Reference data** (changes rarely) is loaded with CLI commands: `tropobeat insert <dataset>`.
- **Weather data** is pulled from Open-Meteo by the beat's main loop (`tropobeat run`). `beater/openmeteo` only fetches and organizes the data. `beater/ducting` does all the ducting calculations.
- **Correlation** runs in the same beat loop, publishing events through the normal libbeat output.
- **Geography** is stored as `geo_point` throughout, so distance, path and map queries can run in Elasticsearch.

## 3. Phases

### Phase 0: Foundation ✅

| Item | Status | Notes |
|---|---|---|
| Beat scaffold renamed `countbeat` → `tropobeat` (code, config, docs, file names) | ✅ | Builds and passes `go vet`. Committed as "tropobeat init" / "logstash tropobeat rename". |
| CLI framework for data-loading commands: `tropobeat insert <subcommand>` | ✅ | One package per command under `cmd/insert/`, registered from `cmd/root.go`. |
| Shared Elasticsearch package `elastic/` | ✅ | Official `go-elasticsearch/v8` client (v8.17.0, same version as libbeat). Provides connection flags (`--es-host`, `--es-username`, `--es-password`, `--es-api-key`, `--es-ca-cert`, `--es-insecure`), `EnsureIndex` and `BulkIndex`. |
| LRF reference model and CSV parser `lrf/` | ✅ | Columns matched by header name; handles Latin-1/Excel encoding, restores ZIP leading zeros, validates coordinates. Unit tested. |
| `tropobeat insert lrfcoordinates` | ✅ | Loads `LRF Addresses_with_GPS_Updated.csv` into index `lrf-coordinates` (211 sites). `location` is a `geo_point`; document ID = LRF code, so re-runs update rather than duplicate. Supports `--recreate` and `--dry-run`. Verified against Elasticsearch 8.17 with a geo-distance query. |
| Commit Phase 0 work | ✅ | Committed to `master` on 2026-10-07 (`f6e3d0a`). |

### Phase 1: Weather data via Open-Meteo 🔄

The prototype is `scripts/Meteo_Script.py`.

| Item | Status | Notes |
|---|---|---|
| `beater/openmeteo` client package | ✅ | `Client` interface with `Hourly` (any hourly variables) and `Profiles` (surface + pressure-level snapshot for one UTC hour). Deduplicates coordinates, sends batches of 50 per request, retries 429/5xx/network errors, and returns partial results alongside a `BatchError`/`CoordinateError`/`APIError`. **Data only:** it converts °C to K but does no ducting math. Unit tests, runnable examples, and a live test (`OPENMETEO_LIVE=1`). |
| Profile contents | ✅ | 8 pressure levels (1000, 975, 950, 925, 900, 850, 800, 700 hPa) × temperature, RH and geopotential height, plus a surface level (2 m temperature and RH, surface pressure, terrain elevation). 27 variables in total. The surface level and 700 hPa were added beyond the script (see Decisions). |
| `openmeteo` section in `tropobeat.yml` | ✅ | `api_key` (commercial plans; switches to `customer-api.open-meteo.com`; keystore recommended), `url`, `models`, `pressure_levels`, `batch_size`, `forecast_days`, `timeout`, `batch_delay`, `max_retries`. The key is redacted from errors. A key combined with the free host is rejected. |
| Regenerate generated assets (`mage update`) | ✅ | `tropobeat.yml` and `tropobeat.reference.yml` regenerated from `_meta/config/*.tmpl`. `include/fields.go` was already current. |
| Coordinate source: the 211 LRFs | ✅ | The beat creates an Elasticsearch client from the new `tropobeat.elasticsearch` config (hosts, username/password or API key, CA cert) and loads every LRF from `lrf_index` (default `lrf-coordinates`) at start-up via `lrf.Load` / `elastic.SearchAll`. It stops with a clear error if the cluster is unreachable, the index is missing, or the index is empty. Verified end-to-end: 211 sites loaded. |
| Open-Meteo client created in the beat | ✅ | `openmeteo.New(config.OpenMeteo)` in `beater.New`, so config errors stop the beat at start-up. Not yet called. |
| Replace the template counter loop in `beater/tropobeat.go` | ⏳ | Scheduled poller that converts the loaded LRFs to coordinates, calls `openmeteo.Profiles` each period and publishes the profiles. |
| Stale-data fallback | 📋 | Following the script: on API failure, reuse the last snapshot for up to 3 h and mark the data as cached. |
| Weather index and mapping | 📋 | Raw profiles per coordinate and hour, with a `geo_point` location, strict mapping. |
| API call budget | 📋 | 27 variables = 2.7 calls per batch request under Open-Meteo's counting rule (each 10 variables counts as one call). Size the poll interval and batches to the plan's limits (Q8, Q9). |
| Test `insert lrfcoordinates` against a secured cluster | 📋 | Carried over. Username/password, API key and CA cert are implemented but only tested with security disabled. |

### Phase 2: FCC transmitter data & co-channel threats 📋

| Item | Status | Notes |
|---|---|---|
| FCC TV facility data: `tropobeat insert fccfacilities` | ❓ | Per station: call sign, facility ID, RF channel, transmitter `geo_point`, ERP, HAAT, antenna pattern if available. Source to confirm (Q2). |
| LRF → received-station map: `tropobeat insert lrfstations` | ❓ | Which stations (call sign + RF channel) each LRF receives. Needed to know which channel each LRF must protect (Q1). |
| Co-channel threat table | 📋 | For each LRF + received channel: every other same-channel transmitter, with distance, bearing and whether it lies beyond the normal radio horizon. Precomputed and stored in its own index. |
| Path sample points | 📋 | For each threat path: LRF, midpoint(s) and transmitter coordinates, fed to `beater/openmeteo` so weather is fetched along the whole path (as sketched in the script's sample input). |

### Phase 3: Tropospheric ducting calculation (`beater/ducting`) 📋

A separate package that consumes `openmeteo.Profile` values. Pure functions, with table-driven tests against known soundings.

| Item | Status | Notes |
|---|---|---|
| Above-ground profile | 📋 | Drop pressure levels at or below the terrain. Open-Meteo extrapolates them: at Denver (1597 m) the 1000–850 hPa levels are underground. Build a height-ordered profile from the surface level plus above-ground levels. |
| Refractivity | 📋 | Vapour pressure e from RH and T (Magnus). N = 77.6·P/T + 3.73×10⁵·e/T². Modified refractivity M = N + 0.157·h. |
| Layer gradients and classification | 📋 | dN/dh and dM/dh per layer. Normal, super-refractive (−79 to −157 N/km), or ducting (≤ −157 N/km, i.e. dM/dh < 0), per ITU-R P.453. |
| Duct properties | 📋 | Base, top, thickness, M-deficit; surface-based vs elevated. |
| Ducting likelihood score | 📋 | A likelihood rather than a yes/no, because 200–250 m level spacing dilutes thin ducts. |
| Frequency-dependent trapping | 📋 | Whether a duct is thick enough to trap a given channel's wavelength. Low VHF needs much thicker ducts than UHF. |
| Path ducting index | 📋 | Combine profiles at the Phase 2 path points into a score per co-channel threat path, now and forecast. |

### Phase 4: RF telemetry integration 📋

| Item | Status | Notes |
|---|---|---|
| Locate existing MER, Pre-FEC BER and RF power data in inSITE | ❓ | Index names, field names, sample rate, and how a reading maps to an LRF code and RF channel (Q4). |
| Baselines per LRF/channel | 📋 | Normal ranges by time of day and season, so drops are measured against what is normal for that site. |
| Interference signature | 📋 | CCI typically shows MER/BER degrading while RF power holds steady or rises. A fade shows power dropping. Use this to tell interference from loss of signal. |

### Phase 5: Correlation engine 📋

| Item | Status | Notes |
|---|---|---|
| Detection rules | 📋 | Telemetry anomaly + interference signature + high path ducting index on a co-channel threat path in the same time window. |
| Confidence scoring and root-cause output | 📋 | Each event names the LRF, channel, likely interfering station, path distance and supporting evidence. |
| Events index and mapping | 📋 | Event start/end, LRF `geo_point`, interferer `geo_point`, scores, links to the telemetry window. |

### Phase 6: Prediction & alerting 📋

| Item | Status | Notes |
|---|---|---|
| Forecast at-risk LRFs | 📋 | Apply the path ducting index to Open-Meteo forecast hours (up to 16 days, via `forecast_days`). |
| Alerting in inSITE | 📋 | Proactive "ducting risk" alerts before an event, and root-cause alerts during one. |

### Phase 7: Visualization in inSITE 📋

| Item | Status | Notes |
|---|---|---|
| Map of LRFs, transmitters and active threat paths | 📋 | Uses the `geo_point` fields. |
| Event timeline and correlation dashboard | 📋 | Telemetry overlaid with the ducting index for each event. |

### Phase 8: Hardening & deployment 📋

| Item | Status | Notes |
|---|---|---|
| Remaining configuration in `tropobeat.yml` | 📋 | Thresholds and index names. Consider letting `insert` commands reuse `output.elasticsearch` from the beat config instead of separate flags. |
| Tests and back-testing | 📋 | Replay known past ducting events to check detection accuracy. |
| Packaging and deployment | 📋 | Beat packaging, secured cluster credentials, Open-Meteo API key in the keystore, runbook. |

## 4. Open questions

| # | Question | Blocks |
|---|---|---|
| Q1 | Where is the list of which stations / RF channels each LRF receives? | Phase 2 threat table |
| Q2 | Which FCC data source should we use: the LMS bulk database download, or another feed already in use? How often should it refresh? | Phase 2 FCC import |
| Q4 | Which indices and fields hold MER, Pre-FEC BER and RF power, and how is each reading tied to an LRF code and channel? | Phase 4 |
| Q5 | Which Elasticsearch version and authentication method does the production inSITE cluster use? | Phase 1 secured test, Phase 8 |
| Q7 | Are there records of past blackout / pixelation incidents we can use to validate detection? | Phase 8 back-testing |
| Q8 | Open-Meteo licence: the free API is non-commercial only (10,000 calls/day), and inSITE use is likely commercial, which needs a paid plan and API key. Tropobeat already supports `openmeteo.api_key`. Who will own the account, and which plan? | Phase 1 production polling |
| Q9 | Polling interval and forecast horizon (for example hourly polling with a 3-day forecast)? These drive the API call budget. | Phase 1 poller, Phase 6 |
| Q10 | Which weather model: `best_match` (default) or HRRR for CONUS? Confirm coverage for the Alaska and Puerto Rico LRFs. | Phase 1 config, Phase 3 accuracy |

## 5. Decisions

| Date | Decision | Reason |
|---|---|---|
| 2026-10-07 | Reference data is loaded by `tropobeat insert <dataset>` subcommands, one Go package per command | Keeps loaders separate from the live beat and easy to extend. |
| 2026-10-07 | Official `go-elasticsearch/v8` client, pinned to v8.17.0 | Same version libbeat already uses, so no changes to shared dependencies. The v8 client works with Elasticsearch 8.x and 9.x. |
| 2026-10-07 | Store all locations as `geo_point`, and use strict mappings | Enables distance/path queries and maps; strict mappings reject unexpected fields instead of silently changing the schema. |
| 2026-10-07 | LRF documents keyed by LRF code | Re-running a load updates sites instead of creating duplicates. |
| 2026-10-07 | Commit the LRF CSV to the repo; commit directly to `master` | Keeps the reference data with the code that loads it (resolves former Q6). Matches the existing single-branch workflow. |
| 2026-10-07 | Weather data comes from the Open-Meteo API, queried directly by tropobeat in batches of coordinates, not from the inSITE Weather App | Resolves former Q3. Open-Meteo provides pressure-level temperature, humidity and height for any coordinate, which is what refractivity calculations need. |
| 2026-10-07 | `beater/openmeteo` is data-only; all ducting calculations live in `beater/ducting` (Phase 3) | Keeps the API client simple and testable, and the physics in pure, separately tested functions. |
| 2026-10-07 | Profiles include the surface level and 700 hPa in addition to the script's 1000–800 hPa levels | A live check showed Open-Meteo extrapolates levels below the terrain (Denver: 1000 hPa at 110 m, 26.8 °C, vs a real 2 m temperature of 11.1 °C). The surface level captures the layer where TV-relevant ducts form, and 700 hPa gives mountain sites at least one gradient. |
| 2026-10-07 | Phases reordered: Open-Meteo is Phase 1, FCC transmitters Phase 2, ducting calculation Phase 3 | The weather pipeline can be built and tested independently of the FCC data, which is blocked on Q1 and Q2. |
| 2026-10-07 | The beat reads reference data through its own `tropobeat.elasticsearch` settings, separate from `output.elasticsearch` | The cluster holding reference data may differ from where events are published, and reference reads may need different credentials. The same `elastic` package backs both the beat and the `insert` CLI flags. |
| 2026-10-07 | LRF sites are loaded once at beat start-up | The list changes rarely (via `insert lrfcoordinates`); restart the beat to pick up changes. Periodic reload can be added if needed. |

## 6. Known issues / tech debt

- Source CSV quirks handled in code: Latin-1 encoding, 11 ZIP codes missing leading zeros, a leading space in one coordinate. Fixing them at the source would be cleaner.
- Elasticsearch authentication options untested against a secured cluster.
- Development uses the free Open-Meteo API, which is non-commercial only. Production needs a commercial key (Q8).
- `elastic.SearchAll` returns at most 10,000 documents (a single search) and errors beyond that. Fine for the 211 LRFs; larger reference sets (e.g. FCC facilities in Phase 2) may need point-in-time paging.

## 7. Change log

| Date | Change |
|---|---|
| 2026-10-07 | Plan created. Phase 0 recorded as complete: rename, `insert` CLI framework, `elastic/` package, LRF model and `insert lrfcoordinates` with `geo_point` mapping. |
| 2026-10-07 | Phase 0 committed to `master`, including the LRF CSV and this plan. Q6 resolved. |
| 2026-10-07 | Weather source set to Open-Meteo. Phases reordered: Phase 1 Open-Meteo, Phase 2 FCC transmitters, new Phase 3 ducting calculation (`beater/ducting`); later phases renumbered 4–8. Built `beater/openmeteo` and the `openmeteo` config section, and ran `mage update`. Q3 resolved; Q8–Q10 added. |
| 2026-10-07 | Git history squashed to a single commit (removes a 124 MB binary from history before the first push). The beat now creates an Elasticsearch client (`tropobeat.elasticsearch`, `lrf_index`) and loads all LRF sites at start-up, and creates the Open-Meteo client. |
