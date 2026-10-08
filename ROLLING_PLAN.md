# Tropobeat Rolling Plan

**Automated Tropospheric Ducting & Telemetry Correlation Engine**

| | |
|---|---|
| Last updated | 2026-10-08 |
| Current phase | Phase 1: Weather data & LRF received channels |
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
 LRF reference data ──┐                                      daily co-channel discovery job
 FCC licensing data ──┴──► reference indices (Elasticsearch) ──► (200 mi geo query + geodesic midpoints)
                                                                        │
                                                                        ▼
                                              path groups (LRF · midpoint · transmitter geo_points)
                                              ├─► in memory: used by every weather fetch
                                              └─► published as the `cochannel` metricset (tropobeat index)
                                                                        │
 Open-Meteo API ◄── batched queries ── beater/openmeteo ◄───────────────┘
                                             │ profiles regrouped per path
                                             ▼
                                       beater/ducting ──► weather / ducting indices ──┐
                                                                                     ├──► correlation ──► events index ──► inSITE dashboards,
 RF telemetry (MER, Pre-FEC BER, power) ──► existing inSITE telemetry indices ──────┘      (beat)                         maps & alerts
```

- **Reference data** (changes rarely) is loaded with CLI commands: `tropobeat insert <dataset>`.
- **Co-channel paths** are discovered by a daily scheduled job inside the beat runtime. The resulting groups of geo points (LRF, midpoint, transmitter) are kept in memory for the weather fetches and published to the tropobeat index as their own `cochannel` metricset.
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

### Phase 1: Weather data & LRF received channels 🔄

The weather prototype is `scripts/Meteo_Script.py`. Phase 1 also establishes which RF channels each LRF receives, from demod telemetry, because the Phase 2 transmitter search depends on it.

| Item | Status | Notes |
|---|---|---|
| `beater/openmeteo` client package | ✅ | `Client` interface with `Hourly` (any hourly variables) and `Profiles` (surface + pressure-level snapshot for one UTC hour). Deduplicates coordinates, sends batches of 50 per request, retries 429/5xx/network errors, and returns partial results alongside a `BatchError`/`CoordinateError`/`APIError`. **Data only:** it converts °C to K but does no ducting math. Unit tests, runnable examples, and a live test (`OPENMETEO_LIVE=1`). |
| Profile contents | ✅ | 8 pressure levels (1000, 975, 950, 925, 900, 850, 800, 700 hPa) × temperature, RH and geopotential height, plus a surface level (2 m temperature and RH, surface pressure, terrain elevation). 27 variables in total. The surface level and 700 hPa were added beyond the script (see Decisions). |
| `openmeteo` section in `tropobeat.yml` | ✅ | `api_key` (commercial plans; switches to `customer-api.open-meteo.com`; keystore recommended), `url`, `models`, `pressure_levels`, `batch_size`, `forecast_days`, `timeout`, `batch_delay`, `max_retries`. The key is redacted from errors. A key combined with the free host is rejected. |
| Regenerate generated assets (`mage update`) | ✅ | `tropobeat.yml` and `tropobeat.reference.yml` regenerated from `_meta/config/*.tmpl`. `include/fields.go` was already current. |
| Coordinate source: the 211 LRFs | ✅ | The beat creates an Elasticsearch client from the new `tropobeat.elasticsearch` config (hosts, username/password or API key, CA cert) and loads every LRF from `lrf_index` (default `lrf-coordinates`) at start-up via `lrf.Load` / `elastic.SearchAll`. It stops with a clear error if the cluster is unreachable, the index is missing, or the index is empty. Verified end-to-end: 211 sites loaded. |
| Received stations and RF channels per LRF, from demod telemetry | 📋 | Query the daily demod indices `log-metric-p-rfdemod-YYYY.MM.DD` (e.g. `log-metric-p-rfdemod-2026.10.08`) through the pattern `log-metric-p-rfdemod-*`, configurable as `tropobeat.demod_index`. A composite aggregation collects the distinct `rfdemod.rf_demod.s_lrf_id` + `i_rf_channel` + `s_input_tag` combinations over a recent window (e.g. the last 24 h), keeping only `s_input_status: Locked`. `s_lrf_id` joins to the LRF code in `lrf-coordinates`, and the call sign is the first token of `s_input_tag` (e.g. `WTHI / WTHI2 (OTA)` → `WTHI`). The result, per LRF, is the list of {RF channel, call sign} it receives, which is the input to the Phase 2 200-mile co-channel search. Refreshed daily as a gocron job (run at start-up, then daily, just before co-channel discovery), so it follows retunes. Demods whose `s_lrf_id` isn't in `lrf-coordinates` are logged, not dropped silently. Tag consistency is Q13. |
| Open-Meteo client created in the beat | ✅ | `openmeteo.New(config.OpenMeteo)` in `beater.New`, so config errors stop the beat at start-up. Not yet called. |
| Job scheduler in the beat: [`github.com/go-co-op/gocron/v2`](https://github.com/go-co-op/gocron) **v2.20.0** | ⏳ | Replaces the template counter loop in `beater/tropobeat.go`. `beater.New` creates one `gocron.NewScheduler(gocron.WithLocation(time.UTC), gocron.WithLogger(…))`, adapting gocron's `Logger` to `logp`. `Run` registers the jobs and calls `Start()`, then waits for the beat context; `Stop` calls `Shutdown()`, which waits for running jobs (bounded by `WithStopTimeout`). Every job uses `WithName`, `WithSingletonMode(gocron.LimitModeReschedule)` so a slow run is never overlapped by the next, and a context derived from the beat's so shutdown cancels in-flight API and Elasticsearch calls. Job errors are logged through `WithEventListeners(gocron.AfterJobRunsWithError(…))`. All scheduled work (received-channel query, co-channel discovery, weather poll) runs as gocron jobs. MIT licence. **Pinned to v2.20.0** (`go get github.com/go-co-op/gocron/v2@v2.20.0`). Verified against that release: it requires Go 1.21.4, so it is compatible with the module's Go 1.23, and every API named here exists in it, including the 4-method `Logger` interface (`Debug/Info/Warn/Error(msg string, args ...any)`). It upgrades the workspace's `github.com/jonboulle/clockwork` from v0.2.2 to v0.5.0. libbeat's rate-limit processor only uses `clockwork.Clock` and `NewRealClock()`, which are unchanged, but confirm with `go build ./...` and `go test ./libbeat/processors/ratelimit/` (in `beats/`) when adding it. |
| Weather poll job | 📋 | A gocron job (`DurationJob(period)`, or `CronJob` for clock-aligned polls such as the top of each hour, Q9) that calls `openmeteo.Profiles` for the loaded LRFs and publishes the profiles. Starts immediately at beat start-up (`WithStartAt(gocron.WithStartImmediately())`). |
| Stale-data fallback | 📋 | Following the script: on API failure, reuse the last snapshot for up to 3 h and mark the data as cached. |
| Weather index and mapping | 📋 | Raw profiles per coordinate and hour, with a `geo_point` location, strict mapping. |
| API call budget | 📋 | 27 variables = 2.7 calls per batch request under Open-Meteo's counting rule (each 10 variables counts as one call). Size the poll interval and batches to the plan's limits (Q8, Q9). |
| Test `insert lrfcoordinates` against a secured cluster | 📋 | Carried over. Username/password, API key and CA cert are implemented but only tested with security disabled. |

### Phase 2: FCC transmitter data & co-channel path discovery 📋

A daily job, scheduled inside the beat runtime, finds all active co-channel transmitters within 200 miles of every LRF. It computes the geodesic midpoint of each LRF–transmitter path and builds a group of the LRF, midpoint and transmitter geo points. The groups are held in memory, where the weather fetches use them directly, and are also published to the tropobeat index as the `cochannel` metricset. The weather poller fetches weather for every point with one batched set of Open-Meteo requests.

| Item | Status | Notes |
|---|---|---|
| FCC transmitters: `tropobeat insert fcctransmitters` | ✅ | Run from Linux cron, outside the beat. Fetches the FCC TV Query `list=4` listing for each state (50 states + DC + PR by default; PR added for the San Juan LRF) and indexes transmitters into `fcc-transmitters` with `location` as a `geo_point`, plus call sign, facility ID, service, status, RF and virtual channel, ERP, HAAT, RCAMSL and licensee. Column mapping checked against the FCC's labelled page for WJAR (test fixture). Default filter: licensed (`LIC`) full-power (`DTV`/`TV`/`DT`); `--services`/`--statuses` override. Doc ID `<facility id>:<lat>,<lon>`, so re-runs update. Honest User-Agent, TLS verified, 1.5 s between requests, retries 429/5xx honouring `Retry-After`, stops on 403. After a fully successful run, transmitters no longer listed are pruned, limited to the fetched states; any failed or empty state blocks pruning and the command exits non-zero. ES credentials from `ES_HOSTS`/`ES_USERNAME`/`ES_PASSWORD`/`ES_API_KEY`. Verified live (RI/CT/MA: 37 stations) and end-to-end against Elasticsearch 8.17. |
| Daily co-channel discovery job | 📋 | A gocron job in the beat runtime, separate from the weather poll job: `CronJob(cochannel.schedule, false)` with `WithStartAt(gocron.WithStartImmediately())` so it also runs at beat start-up, and singleton mode so runs never overlap. Input: the received stations and RF channels per LRF (Phase 1). For each LRF and received `rf_channel`, it runs an Elasticsearch `geo_distance` query with a 200-mile radius around the LRF's `geo_point` on the `fcc-transmitters` index, filtered to active transmitters (Q11) on that channel. **Own-station exclusion:** drop the transmitter on that channel whose call sign, ignoring a `-TV`/`-DT`/`-CD`/`-LD` suffix, matches the call sign from `s_input_tag`. If the tag doesn't parse or matches nothing, drop the nearest transmitter on that channel and log the LRF, channel and tag so the tag can be fixed. What remains is the co-channel threat set. Example: TERH on RF 10 excludes WTHI-TV (15.4 mi away). Config: `cochannel.schedule` (cron expression, default `"0 3 * * *"`, i.e. 03:00 UTC daily), `cochannel.radius` (default `200mi`). |
| Geodesic midpoint | 📋 | For every transmitter returned, compute the midpoint of the LRF–transmitter path, plus path distance and bearing, using [`github.com/tidwall/geodesic`](https://github.com/tidwall/geodesic) (Go port of Karney's GeographicLib, MIT, no dependencies) on the WGS84 ellipsoid. `geodesic.WGS84.Inverse(lrfLat, lrfLon, txLat, txLon, &dist, &azi1, nil)` gives the distance (m) and the bearing from the LRF; `geodesic.WGS84.Direct(lrfLat, lrfLon, azi1, dist/2, &midLat, &midLon, nil)` gives the midpoint. Wrapped in a small pure function, unit-tested against known values (the library's README example: ≈165,330 m between 33.4911, −112.4223 and 32.1189, −113.1123) and checked that the midpoint is equidistant from both ends. The dependency is added to `go.mod` when this item is built. |
| Path groups: in memory + `cochannel` metricset | 📋 | Each discovery run builds one group per LRF–transmitter pair: LRF (code, `geo_point`), midpoint (`geo_point`), transmitter (facility ID, call sign, RF channel, `geo_point`), plus distance, bearing and a run ID/timestamp. **In memory:** the run atomically replaces the beat's current set of groups, and the weather poller reads that set on every fetch, so no Elasticsearch query is needed per poll. **Published:** each group is also published through the beat output as an event with `metricset.name: cochannel` in the tropobeat index. That gives a daily, time-stamped snapshot for dashboards, maps and later correlation. Because events are appended rather than upserted, no pruning is needed; consumers use the latest run (`run_id`). **Start-up:** memory is empty when the beat starts, so the discovery job runs immediately (gocron `WithStartImmediately`), then on its schedule. Until the first run finishes, the weather poll job falls back to LRF-only points. **Mapping:** the fields are declared in `_meta/fields.yml` under `tropobeat.cochannel.*`, with `geo_point` for the three locations, so the beat's index template maps them; regenerate with `mage update`. Replaces the earlier plan for a separate `lrf-cochannel-vectors` index. |
| Grouped weather queries: `openmeteo.Client.GroupProfiles` | ✅ | Accepts a list of groups, each with an ID and labelled points (e.g. `lrf`, `midpoint`, `transmitter`). Coordinates from all groups are deduplicated and batched together, so an LRF shared by many paths, or a transmitter near several LRFs, is fetched once. Profiles are returned regrouped in input order, and `Complete()` reports whether every point in a group has data. Unit tests and a live test (`OPENMETEO_LIVE=1`) using the script's sample paths. |
| Weather poller reads path groups | 📋 | The Phase 1 poller switches from LRF-only coordinates to the in-memory path groups from the latest discovery run, calls `openmeteo.GroupProfiles`, and publishes weather per path. |
| API call budget with paths | 📋 | Total points = LRFs + unique transmitters + one midpoint per pair, so it grows with the number of co-channel pairs. Deduplication helps, but the count drives the poll interval and plan size (Q8, Q9). Confirm how Open-Meteo counts multi-coordinate requests. |

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
| Path ducting index | 📋 | Combine the LRF, midpoint and transmitter profiles of each `GroupProfile` (Phase 2) into a score per co-channel path, now and forecast. |

### Phase 4: RF telemetry integration 📋

| Item | Status | Notes |
|---|---|---|
| Demod telemetry in inSITE | 📋 | Daily indices `log-metric-p-rfdemod-YYYY.MM.DD`, queried as `log-metric-p-rfdemod-*`. Fields under `rfdemod.rf_demod`: `i_input_mer` (MER, dB), `d_pre_fec_ber` (Pre-FEC BER), `i_input_power` (input power, dBm), `s_input_status` (lock status), `i_rf_channel`, `s_input_tag`, `s_lrf_id` (LRF code), and card identity `s_card_label`, `s_frame_label`, `s_demod_side`, `i_slot`, `i_input`. Not used: `as_ids`, the collector's internal API IDs for polling the demod, which don't relate to anything else. Sample rate and retention to be measured from the data. The same data feeds the Phase 1 received-channel query. |
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
| Q5 | Which Elasticsearch version and authentication method does the production inSITE cluster use? | Phase 1 secured test, Phase 8 |
| Q7 | Are there records of past blackout / pixelation incidents we can use to validate detection? | Phase 8 back-testing |
| Q8 | Open-Meteo licence: the free API is non-commercial only (10,000 calls/day), and inSITE use is likely commercial, which needs a paid plan and API key. Tropobeat already supports `openmeteo.api_key`. Who will own the account, and which plan? | Phase 1 production polling |
| Q9 | Polling interval and forecast horizon (for example hourly polling with a 3-day forecast)? These drive the API call budget. | Phase 1 poller, Phase 6 |
| Q10 | Which weather model: `best_match` (default) or HRRR for CONUS? Confirm coverage for the Alaska and Puerto Rico LRFs. | Phase 1 config, Phase 3 accuracy |
| Q11 | Which FCC records count as an "active" transmitter beyond the default (licensed full-power `DTV`/`TV`/`DT`, PR included)? Should construction permits, Class A (`DCA`), LPTV (`LPD`), distributed transmission sites (`DTS`) or replacement translators (`DRT`) be added? All can be enabled with `--services`/`--statuses` without code changes. | Phase 2 discovery filter |
| Q13 | Is `s_input_tag` consistently formatted so that its first token is always a call sign that maps to an `fcc-transmitters` record? The one sample (`WTHI / WTHI2 (OTA)` → `WTHI-TV`) fits, but the tag is hand-entered free text. Are there tags using network names, virtual channel numbers, satellite/IP feed names, or non-full-power call signs (`-LD`, `-CD`, translators)? A one-off audit would answer it: distinct `s_input_tag` values per `s_lrf_id` across `log-metric-p-rfdemod-*`, each checked against `fcc-transmitters`. Tags that don't match fall back to nearest-on-channel, which is less reliable. | Phase 1 received-channel accuracy, Phase 2 own-station exclusion |


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
| 2026-10-08 | Co-channel paths are discovered by a daily background job: a 200-mile geo query per LRF on the FCC index, then a geodesic midpoint per transmitter, stored as LRF/midpoint/transmitter groups in `lrf-cochannel-vectors` | Transmitter licences change slowly, so a daily run keeps the paths current without repeating spatial queries on every weather poll. |
| 2026-10-08 | Midpoints, path distances and bearings use `github.com/tidwall/geodesic` on the WGS84 ellipsoid | A true ellipsoidal geodesic (GeographicLib algorithms) from a small, dependency-free MIT library, with distance and bearing from the same call. |
| 2026-10-08 | `openmeteo` accepts groups of labelled points (`GroupProfiles`) and returns weather regrouped, while staying generic (no FCC/LRF types) | Batching and deduplication across all paths minimises API calls. Generic labels keep the weather package independent of the discovery logic. |
| 2026-10-08 | FCC transmitter data comes from the FCC TV Query `list=4` text output, polled per state by `tropobeat insert fcctransmitters` from cron; requests use an honest User-Agent with TLS verification and pacing, with no browser impersonation | Resolves former Q2. The endpoint answers normal, identified requests, so evading its protections is neither needed nor appropriate. Running outside the beat keeps the slow nightly fetch separate from live polling. |
| 2026-10-08 | Default FCC filter kept at licensed full-power (`DTV`/`TV`/`DT`); `DTS` and `DRT` deliberately excluded | Matches the prototype's "High Power Only" intent. See Known issues for what this leaves out. |
| 2026-10-08 | The stations and RF channels each LRF receives are derived from demod telemetry (station ID + RF channel per demod), correlated to `lrf-coordinates`, rather than maintained as a separate list | Resolves former Q1. The demods report what each LRF actually receives, so the map stays current without manual upkeep. |
| 2026-10-08 | An LRF's own stations are identified from demod telemetry: `s_lrf_id` gives the LRF, `i_rf_channel` the RF channel, and the first token of `s_input_tag` the call sign. They are matched to `fcc-transmitters` by RF channel + call sign (suffix-insensitive), with a logged nearest-on-channel fallback | Resolves former Q12. The demods report no FCC facility ID or TSID, and call sign + RF channel identifies the wanted station uniquely within 200 mi. Reliability depends on tag consistency (Q13). |
| 2026-10-08 | Demod telemetry is read from the daily `log-metric-p-rfdemod-YYYY.MM.DD` indices via `log-metric-p-rfdemod-*` (configurable as `tropobeat.demod_index`); fields are under `rfdemod.rf_demod.*`, linked to the LRF by `s_lrf_id`. `as_ids` is ignored | Resolves former Q4. `as_ids` are internal collector API IDs with no meaning outside polling. |
| 2026-10-08 | Collecting each LRF's received stations and RF channels from demod telemetry moves from Phase 2 to Phase 1 | Phase 2's 200-mile co-channel search needs each LRF's received RF channels as its input. |
| 2026-10-08 | Co-channel path groups are kept in memory for the weather poller and published to the tropobeat index as their own `cochannel` metricset, instead of a separate `lrf-cochannel-vectors` index | Discovery runs on a schedule inside the beat, so the poller can use the groups straight from memory with no per-poll query. Publishing through the beat output reuses its index, template and retention, and gives a time-stamped daily snapshot without upserts or pruning. Supersedes the `lrf-cochannel-vectors` part of the earlier discovery-job decision. |
| 2026-10-08 | All scheduled work inside the beat (received-channel query, co-channel discovery, weather polling) runs as jobs on a single `github.com/go-co-op/gocron/v2` scheduler, pinned to v2.20.0 | Cron and daily schedules, run-at-start, singleton mode (no overlapping runs), error listeners and graceful shutdown out of the box, instead of hand-rolled tickers. MIT licence. |
| 2026-10-07 | LRF sites are loaded once at beat start-up | The list changes rarely (via `insert lrfcoordinates`); restart the beat to pick up changes. Periodic reload can be added if needed. |

## 6. Known issues / tech debt

- Source CSV quirks handled in code: Latin-1 encoding, 11 ZIP codes missing leading zeros, a leading space in one coordinate. Fixing them at the source would be cleaner.
- Elasticsearch authentication options untested against a secured cluster.
- Development uses the free Open-Meteo API, which is non-commercial only. Production needs a commercial key (Q8).
- The default FCC filter excludes `DTS` sites and `DRT` translators. Some full-power stations exist only as `DTS` rows (e.g. KCWX, KVMD, KQSL in a 6-state sample), and some `DRT`s run 10–15 kW on other channels, so these transmitters are not in the index unless `--services DTV,TV,DT,DTS,DRT` is used (Q11).
- The FCC TV Query is an older endpoint with no published API contract. If its format changes, the parser fails loudly (column-count check) rather than importing bad data. The FCC LMS bulk database download is the fallback source.
- `elastic.SearchAll` returns at most 10,000 documents (a single search) and errors beyond that. Fine for the 211 LRFs; larger reference sets (e.g. FCC facilities in Phase 2) may need point-in-time paging.

## 7. Change log

| Date | Change |
|---|---|
| 2026-10-07 | Plan created. Phase 0 recorded as complete: rename, `insert` CLI framework, `elastic/` package, LRF model and `insert lrfcoordinates` with `geo_point` mapping. |
| 2026-10-07 | Phase 0 committed to `master`, including the LRF CSV and this plan. Q6 resolved. |
| 2026-10-07 | Weather source set to Open-Meteo. Phases reordered: Phase 1 Open-Meteo, Phase 2 FCC transmitters, new Phase 3 ducting calculation (`beater/ducting`); later phases renumbered 4–8. Built `beater/openmeteo` and the `openmeteo` config section, and ran `mage update`. Q3 resolved; Q8–Q10 added. |
| 2026-10-07 | Git history squashed to a single commit (removes a 124 MB binary from history before the first push). The beat now creates an Elasticsearch client (`tropobeat.elasticsearch`, `lrf_index`) and loads all LRF sites at start-up, and creates the Open-Meteo client. |
| 2026-10-08 | Phase 2 redefined around a daily co-channel discovery job (200 mi geo query, geodesic midpoints, `lrf-cochannel-vectors` lookup store of grouped geo points). Added `openmeteo.Client.GroupProfiles` for batched, regrouped weather queries. Q11 added. |
| 2026-10-08 | Geodesic midpoint calculation set to use `github.com/tidwall/geodesic` (WGS84) instead of a spherical great-circle formula. |
| 2026-10-08 | Built `tropobeat insert fcctransmitters` (cron job) and the `fcc` package: FCC TV Query fetch, parser, `fcc-transmitters` index with `geo_point` locations, state-scoped pruning. Added `elastic.DeleteByQuery` and `ES_*` environment credentials for the insert commands. Q2 resolved; Q11 narrowed. |
| 2026-10-08 | Q1 resolved: received stations/RF channels per LRF come from demod telemetry, correlated to `lrf-coordinates`. Phase 2 map row and Phase 4 telemetry row updated; Q4 widened to cover demod station ID/RF channel; Q12 added (matching demod station IDs to FCC records). |
| 2026-10-08 | Q12 resolved (demod `s_lrf_id` / `i_rf_channel` / `s_input_tag` matched to FCC by RF channel + call sign). Collecting each LRF's received stations and RF channels from demod telemetry moved from Phase 2 to Phase 1. Q4 resolved (daily `log-metric-p-rfdemod-YYYY.MM.DD` indices). Q13 added (is `s_input_tag` always an FCC-matchable call sign?). Phase 4 telemetry fields recorded. |
| 2026-10-08 | Co-channel path groups moved from a separate `lrf-cochannel-vectors` index to in-memory storage plus a `cochannel` metricset published to the tropobeat index. Discovery runs at beat start-up and then on its schedule; field mappings to be declared in `_meta/fields.yml`. |
| 2026-10-08 | Scheduling inside the beat set to `github.com/go-co-op/gocron/v2`: one scheduler with jobs for the received-channel query, co-channel discovery (`cochannel.schedule` cron, run at start-up) and weather polling. Replaces the plain ticker loop; `cochannel.interval` replaced by `cochannel.schedule`. |
| 2026-10-08 | gocron pinned to v2.20.0 (requires Go 1.21.4; all planned APIs present; raises `jonboulle/clockwork` to v0.5.0, to be checked against libbeat's rate-limit processor). |
