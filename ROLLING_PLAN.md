# Tropobeat Rolling Plan

**Automated Tropospheric Ducting & Telemetry Correlation Engine**

| | |
|---|---|
| Last updated | 2026-10-13 |
| Current phase | Phases 1 and 2 core complete; next up: Phase 3 ducting calculation |
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
 INPUTS
 ┌─────────────────────┐   ┌─────────────────────┐   ┌─────────────────────┐
 │      LRF list       │   │  FCC transmitters   │   │   Demod telemetry   │
 │  (lrf-coordinates)  │   │ (fcc-transmitters)  │   │ (channels received) │
 └──────────┬──────────┘   └──────────┬──────────┘   └──────────┬──────────┘
            │                         │                         │
            └─────────────────────────┼─────────────────────────┘
                                      │
                                      ▼
                    ┌───────────────────────────────────┐
                    │  Daily co-channel discovery       │
                    │  (inside tropobeat)               │
                    │                                   │
                    │  • 200-mile same-channel search   │
                    │  • own station excluded           │
                    │  • geodesic midpoints             │
                    └─────────────────┬─────────────────┘
                                      │
                                      ▼
                    ┌───────────────────────────────────┐
                    │  Co-channel paths                 │
                    │  LRF · midpoint · transmitter     │
                    └─────────────────┬─────────────────┘
                                      │
                  ┌───────────────────┴───────────────────┐
                  │                                       │
                  ▼                                       ▼
       ┌─────────────────────┐             ┌─────────────────────────────┐
       │  Kept in memory     │             │  Published as the           │
       │  for weather polls  │             │  `cochannel` metricset      │
       └──────────┬──────────┘             └─────────────────────────────┘
                  │
                  ▼
       ┌─────────────────────┐
       │  Hourly weather     │◄──────── batched requests ──────── Open-Meteo API
       │  poll               │
       └──────────┬──────────┘
                  │  weather per path point
                  ▼
       ┌─────────────────────┐             ┌─────────────────────────────┐
       │  Ducting            │             │  Demod telemetry            │
       │  calculation        │             │  (MER, Pre-FEC BER, power)  │
       └──────────┬──────────┘             └──────────────┬──────────────┘
                  │                                       │
                  └───────────────────┬───────────────────┘
                                      │
                                      ▼
                    ┌───────────────────────────────────┐
                    │  Correlation  →  events           │
                    └─────────────────┬─────────────────┘
                                      │
                                      ▼
                    ┌───────────────────────────────────┐
                    │  inSITE dashboards, maps & alerts │
                    └───────────────────────────────────┘
```

- **Reference data** that changes rarely (the LRF list, FCC transmitters) is loaded with the `tropobeat insert` commands, which can run from cron while tropobeat is running.
- **Co-channel paths** are discovered daily by tropobeat itself and kept in memory for the weather polls. They are also published to the tropobeat index as the `cochannel` metricset, for dashboards and maps.
- **Weather data** comes from Open-Meteo. The `internal/openmeteo` package only fetches and organises it; the ducting calculation (`internal/ducting`) interprets it.
- **Correlation** and alerting will run inside tropobeat and publish through its normal output.
- **Locations** are stored as geographic points (and co-channel paths as lines), so distance queries and Kibana maps work directly.

## 3. Phases

### Phase 0: Foundation ✅

| Item | Status | Notes |
|---|---|---|
| Project set up as an Elastic Beat named tropobeat | ✅ | Based on the standard beat template, renamed from its placeholder name. |
| Data-loading commands: `tropobeat insert …` | ✅ | A command group for loading reference data, separate from the running beat. |
| Elasticsearch connectivity | ✅ | Uses the official Elasticsearch Go client. Connection options cover hosts, username/password or API key, a custom CA certificate, and (for testing only) skipping certificate checks. |
| LRF list loader | ✅ | Reads the LRF spreadsheet export. Copes with its quirks (Excel text encoding, ZIP codes missing leading zeros, stray spaces) and validates every coordinate. |
| `tropobeat insert lrfcoordinates` | ✅ | Loads the 211 LRFs into the `lrf-coordinates` index with each site's location as a geographic point. Re-running updates sites rather than duplicating them. Supports a preview mode (`--dry-run`) and rebuilding the index (`--recreate`). Verified against Elasticsearch 8. |
| Phase 0 committed | ✅ | 2026-10-07. |

### Phase 1: Weather data & LRF received channels 🔄

Tropobeat fetches weather from Open-Meteo, and works out which RF channels each LRF receives from the demod telemetry, because the Phase 2 transmitter search depends on it. The original weather prototype is `scripts/Meteo_Script.py`.

| Item | Status | Notes |
|---|---|---|
| Weather client (`internal/openmeteo`) | ✅ | Fetches weather for many coordinates at once, sending them in batches and fetching each unique location only once. Retries on rate limits and server errors, and returns whatever succeeded if part of a request fails. It only organises the data (temperatures in Kelvin); it does no ducting calculations. |
| Weather contents | ✅ | For each location and hour: 8 pressure levels (1000, 975, 950, 925, 900, 850, 800 and 700 hPa) with temperature, relative humidity and height, plus the surface (2 m temperature and humidity, surface pressure, terrain elevation). The surface level and 700 hPa go beyond the prototype (see Decisions). |
| Weather settings in `tropobeat.yml` | ✅ | `openmeteo:` section: `api_key` (commercial plans; the key is kept out of logs and error messages), `url`, `models`, `pressure_levels`, `batch_size`, `forecast_days`, `timeout`, `batch_delay`, `max_retries`. |
| LRF list read at start-up | ✅ | Tropobeat reads all LRFs from `lrf-coordinates` (settings `tropobeat.elasticsearch` and `lrf_index`). It stops with a clear message if the cluster is unreachable or the index is missing or empty. Verified with all 211 sites. |
| Received stations and RF channels per LRF, from demod telemetry | ✅ | Reads the demod telemetry (`log-metric-p-rfdemod-*`, setting `demod_index`) for the last 24 hours (`cochannel.demod_window`), keeping only locked inputs. For each LRF it lists the RF channels received and the call signs in each input's tag (e.g. `WTHI / WTHI2 (OTA)` → WTHI, WTHI2; Mexican stations such as XHAS are recognised too). Runs together with co-channel discovery as one daily job, so discovery always uses fresh channel data. Demod LRF IDs that aren't in the LRF list are logged and skipped (Q14). Verified against a day of real demod telemetry: 201 LRFs, 1,091 locked channels. |
| Scheduler inside tropobeat | ✅ | All recurring work runs on a gocron scheduler: co-channel discovery daily at 03:00 UTC and at start-up; the weather poll every hour, with its first poll started as soon as the first discovery run finishes, so it covers the co-channel paths from the outset. A slow run is never overlapped by the next one, and shutting tropobeat down waits for running work to finish. Replaces the template's placeholder loop. |
| Weather poll | ✅ | Fetches weather for every co-channel path point (LRF, midpoint, transmitter) and for LRFs with no paths, and publishes one `weather` event per path or LRF. Interval set by `period` (default 1 hour). If discovery fails, it polls the LRFs only. Each run logs how many points were requested, how many unique locations were fetched and how many requests were sent. Verified with a limited run against the live Open-Meteo API (3 LRFs: 49 paths, 147 points, 80 unique locations, 2 requests). |
| Limit to chosen LRFs: `lrf_sites` | ✅ | Optional list of LRF codes (empty = all). Limits discovery and weather collection to those sites, for limited runs and practical testing, and to control API usage during trials (Q15). Codes are case-insensitive; unknown codes are logged; if none are valid tropobeat stops with a clear message. Example: `lrf_sites: ["TERH", "PROV"]`. |
| Optional merging of nearby weather points: `coordinate_precision` | ✅ | Off by default. When set (e.g. `0.01`° ≈ 1 km), points that fall in the same weather-model grid cell are fetched once. Measured on the full network: 2,589 unique locations without merging, 2,390 at `0.01` (−8%) and 2,142 at `0.05` (−17%) (Q15). |
| Weather polling on/off: `openmeteo.enabled` | ✅ | On by default. Set `enabled: false` in the `openmeteo` section to run co-channel discovery only: no weather polls, no `weather` events and no Open-Meteo calls (the API key isn't needed). Useful for running discovery in production before the Open-Meteo plan is settled (Q15). |
| Stale-data fallback | 📋 | Following the prototype: if Open-Meteo is unavailable, reuse the last weather for up to 3 hours and mark it as cached. |
| API call budget | 📋 | See Q15. |
| Test the insert commands against a secured cluster | 📋 | Username/password, API key and CA certificate are supported but have only been tested with security disabled (Q5). |

### Phase 2: FCC transmitter data & co-channel path discovery ✅

Once a day, inside tropobeat, every LRF's received channels are searched for same-channel FCC transmitters within 200 miles. The LRF's own station is excluded, the geodesic midpoint of each remaining path is calculated, and the LRF, midpoint and transmitter are kept together as a **co-channel path**. The paths are held in memory for the weather polls and published as the `cochannel` metricset.

| Item | Status | Notes |
|---|---|---|
| FCC transmitters: `tropobeat insert fcctransmitters` | ✅ | Run from Linux cron, outside tropobeat. Downloads the FCC TV station listing state by state (50 states, DC and PR; PR added for the San Juan LRF) into the `fcc-transmitters` index: call sign, facility ID, service, RF and virtual channel, power, antenna height, licensee and location. The column layout was checked against the FCC's own station page. Default: licensed full-power stations only (overridable). Requests identify tropobeat honestly, are paced, and back off when asked; a refused request stops the run. Stations no longer listed are removed only after a fully successful run, and only for the states fetched. Credentials can come from environment variables. Verified live and against Elasticsearch 8. |
| Daily co-channel discovery | ✅ | Runs daily at 03:00 UTC (`cochannel.schedule`) and at start-up. Re-reads the LRF list each run, so LRF changes are picked up without a restart. For each LRF and received channel, finds same-channel transmitters within 200 miles (`cochannel.radius`). **Own-station exclusion:** a transmitter whose call sign matches the input's tag (ignoring suffixes such as `-TV`) is the LRF's own station and is excluded. If the tagged station isn't in the full-power FCC set (LPTV, Class A, translators, foreign stations), nothing is excluded, because every result is a genuine full-power transmitter. Inputs with no call sign (`Spare`, `NA`, test inputs) are skipped and logged. The own station is looked up nationwide, and if it is more than 150 miles from the LRF the channel is skipped with a warning, since that points to an LRF code or coordinate mismatch (Q14). What remains are the LRF's co-channel paths. Example: TERH on RF 10 excludes WTHI-TV (15.4 miles away). Verified on the full network with real demod data: 1,091 channels searched, 1,001 own-station transmitters excluded, 1,829 co-channel paths; the MON/MONR code swap is detected on every affected channel; an LRF moved with `insert lrfcoordinates` was picked up by the next run without a restart. |
| Geodesic midpoints | ✅ | Midpoint, distance and bearing of each path, calculated on the WGS84 ellipsoid with tidwall/geodesic. |
| Co-channel paths in memory and in the `cochannel` metricset | ✅ | Each run replaces the in-memory paths in one step, and publishes one event per path: LRF, midpoint and transmitter locations, channel, distance, bearing and run ID, plus the path itself as a line so Kibana Maps can draw every path in one layer. Events are appended per run, giving a dated daily snapshot; consumers use the latest run. Locations are mapped as geographic points and the path as a geographic shape; verified that a map-area search returns the expected paths. |
| Grouped weather requests | ✅ | The weather client accepts groups of labelled points (LRF, midpoint, transmitter), fetches all points together with duplicates removed, and returns the weather grouped back per path. |
| Weather poll uses the co-channel paths | ✅ | See the Phase 1 weather poll. |
| API call budget with paths | 📋 | See Q15. Measured on the full network: 1,829 co-channel paths, 5,512 path points, 2,589 unique weather locations per poll. |

### Phase 3: Tropospheric ducting calculation (`internal/ducting`) 📋

Turns the weather for each path point into ducting indicators. Kept separate from the weather client and tested against known soundings.

| Item | Status | Notes |
|---|---|---|
| Above-ground profile | 📋 | Drop pressure levels at or below the terrain. Open-Meteo extrapolates them: at Denver (1597 m) the 1000–850 hPa levels are underground. Build a height-ordered profile from the surface level plus above-ground levels. |
| Refractivity | 📋 | Vapour pressure e from RH and T (Magnus). N = 77.6·P/T + 3.73×10⁵·e/T². Modified refractivity M = N + 0.157·h. |
| Layer gradients and classification | 📋 | dN/dh and dM/dh per layer. Normal, super-refractive (−79 to −157 N/km), or ducting (≤ −157 N/km, i.e. dM/dh < 0), per ITU-R P.453. |
| Duct properties | 📋 | Base, top, thickness, M-deficit; surface-based vs elevated. |
| Ducting likelihood score | 📋 | A likelihood rather than a yes/no, because 200–250 m level spacing dilutes thin ducts. |
| Frequency-dependent trapping | 📋 | Whether a duct is thick enough to trap a given channel's wavelength. Low VHF needs much thicker ducts than UHF. |
| Path ducting index | 📋 | Combine the LRF, midpoint and transmitter weather of each co-channel path into one score, now and forecast. |

### Phase 4: RF telemetry integration 📋

| Item | Status | Notes |
|---|---|---|
| Demod telemetry in inSITE | 📋 | Daily indices `log-metric-p-rfdemod-YYYY.MM.DD`. Fields under `rfdemod.rf_demod`: `i_input_mer` (MER, dB), `d_pre_fec_ber` (Pre-FEC BER), `i_input_power` (input power, dBm), `s_input_status` (lock status), `i_rf_channel`, `s_input_tag`, `s_lrf_id` (LRF code), and card identity `s_card_label`, `s_frame_label`, `s_demod_side`, `i_slot`, `i_input`. `as_ids` (the collector's internal polling IDs) is not used. Each input reports about once a minute. The same data feeds the Phase 1 received-channel list. |
| Baselines per LRF/channel | 📋 | Normal ranges by time of day and season, so drops are measured against what is normal for that site. |
| Interference signature | 📋 | CCI typically shows MER/BER degrading while RF power holds steady or rises. A fade shows power dropping. Use this to tell interference from loss of signal. |

### Phase 5: Correlation engine 📋

| Item | Status | Notes |
|---|---|---|
| Detection rules | 📋 | Telemetry anomaly + interference signature + high path ducting index on a co-channel path in the same time window. |
| Confidence scoring and root-cause output | 📋 | Each event names the LRF, channel, likely interfering station, path distance and supporting evidence. |
| Event records | 📋 | Event start/end, LRF and interferer locations, scores, and a link to the telemetry window. |

### Phase 6: Prediction & alerting 📋

| Item | Status | Notes |
|---|---|---|
| Forecast at-risk LRFs | 📋 | Apply the path ducting index to Open-Meteo forecast hours (up to 16 days ahead, setting `forecast_days`). |
| Alerting in inSITE | 📋 | Proactive "ducting risk" alerts before an event, and root-cause alerts during one. |

### Phase 7: Visualization in inSITE 📋

| Item | Status | Notes |
|---|---|---|
| Map of LRFs, transmitters and co-channel paths | 📋 | Co-channel paths drawn as lines, highlighting those with active ducting. |
| Event timeline and correlation dashboard | 📋 | Telemetry overlaid with the ducting index for each event. |

### Phase 8: Hardening & deployment 📋

| Item | Status | Notes |
|---|---|---|
| Remaining configuration in `tropobeat.yml` | 📋 | Thresholds and index names. Consider letting the insert commands reuse tropobeat's output connection settings. |
| Tests and back-testing | 📋 | Replay known past ducting events to check detection accuracy. |
| Packaging and deployment | 📋 | Packaging, secured cluster credentials, the Open-Meteo API key in the keystore, and a runbook. |

## 4. Open questions

| # | Question | Blocks |
|---|---|---|
| Q5 | Which Elasticsearch version and authentication method does the production inSITE cluster use? | Phase 1 secured test, Phase 8 |
| Q7 | Are there records of past blackout / pixelation incidents we can use to validate detection? | Phase 8 back-testing |
| Q8 | Open-Meteo licence: the free API is non-commercial only (10,000 calls/day), and inSITE use is likely commercial, which needs a paid plan and API key. Tropobeat already supports `openmeteo.api_key`. Who will own the account, and which plan? See Q15 for the expected usage. | Production weather polling |
| Q9 | Polling interval and forecast horizon (currently hourly polling)? These drive the API usage in Q15. | Phase 1 poller, Phase 6 |
| Q10 | Which weather model: `best_match` (default) or HRRR for CONUS? Confirm coverage for the Alaska and Puerto Rico LRFs. | Phase 1 config, Phase 3 accuracy |
| Q11 | Which FCC records count as an "active" transmitter beyond the default (licensed full-power `DTV`/`TV`/`DT`, PR included)? Should construction permits, Class A (`DCA`), LPTV (`LPD`), distributed transmission sites (`DTS`) or replacement translators (`DRT`) be added? All can be enabled with `--services`/`--statuses`. | Phase 2 discovery filter |
| Q13 | Should demod input tags be standardised at source? Real data (2026-10-13), 1,095 locked channels at known LRFs: 962 (about 88%) have tags matching a full-power FCC station by call sign; 118 (about 11%) name LPTV, Class A, translator or Mexican stations (handled: nothing excluded); 4 are spare, test or empty inputs (skipped); 11 belong to the swapped MON/MONR codes (Q14). The tags vary in spacing and `(OTA)` style (`KOLR (OTA)` vs `KOLR ( OTA )`, `WMTW(OTA)`), which tropobeat tolerates but which a naming standard would make more reliable. | Phase 2 own-station exclusion accuracy |
| Q14 | Seven LRFs use different codes in the demod telemetry than in `lrf-coordinates`: `BIRM`/`BIR`, `HTSBG`/`HSTBG`, `LUB`/`LUBB`, `PRESQ`/`PRSQ`, `RNO`/`RNC`, and `MON`/`MONR` are swapped (in the LRF list `MON` is Monroe LA; in the demods `MON` is Monterey CA). Which codes are authoritative: fix the LRF list, fix the demod labels, or add a translation table in tropobeat? Until resolved, these sites are skipped by co-channel discovery: the five mismatched codes as unknown LRF IDs, and the MON/MONR channels by the 150-mile own-station check (verified). Also, `GLEN`, `GRNNC`, `VICT` and `WATER` had no demod data in the 2026-10-13 sample. | Phase 2 discovery for those 11 sites |
| Q15 | Open-Meteo API budget. Open-Meteo counts calls per location, and a request with more than 10 weather variables counts as several calls. Measured on the full network (2026-10-13): 1,829 co-channel paths give 5,512 path points, of which 2,589 are unique locations; at 27 variables each poll costs about 7,000 calls. Hourly polling is about 5.0 million calls a month, right at the Professional plan's 5 million limit (with `coordinate_precision` 0.01: about 4.6 million; 0.05: about 4.2 million; every 3 hours: about 1.7 million; every 6 hours: about 0.8 million). Which plan will be purchased, and should usage be reduced by polling less often (one poll's hourly forecast covers the hours in between), requesting fewer variables or pressure levels, or merging nearby points (`coordinate_precision`)? During trials, `lrf_sites` limits usage to chosen LRFs, and `openmeteo.enabled: false` turns weather polling off entirely while co-channel discovery keeps running. | Production weather polling |

## 5. Decisions

| Date | Decision | Reason |
|---|---|---|
| 2026-10-07 | Reference data is loaded with `tropobeat insert` commands | Keeps loading separate from the running beat and easy to extend. |
| 2026-10-07 | Elasticsearch access uses the official Elasticsearch Go client, the same one the Beats framework uses | Works with Elasticsearch 8 and 9, and adds no conflicting dependencies. |
| 2026-10-07 | All locations are stored as geographic points, with strict index mappings | Enables distance queries and maps; strict mappings reject unexpected fields instead of silently changing the schema. |
| 2026-10-07 | LRF records are keyed by LRF code | Re-running a load updates sites instead of creating duplicates. |
| 2026-10-07 | The LRF spreadsheet export is kept in the repository | Keeps the reference data with the code that loads it (resolves former Q6). |
| 2026-10-07 | Weather data comes from the Open-Meteo API, queried directly by tropobeat, not from the inSITE Weather App | Resolves former Q3. Open-Meteo provides pressure-level temperature, humidity and height for any coordinate, which is what refractivity calculations need. |
| 2026-10-07 | The weather client only fetches and organises data; all ducting calculations live in a separate ducting package (Phase 3) | Keeps each part simple and independently testable. |
| 2026-10-07 | Weather includes the surface level and 700 hPa in addition to the prototype's 1000–800 hPa levels | A live check showed Open-Meteo extrapolates levels below the terrain (Denver: 1000 hPa at 110 m, 26.8 °C, vs a real 2 m temperature of 11.1 °C). The surface level captures the layer where TV-relevant ducts form, and 700 hPa gives mountain sites at least one gradient. |
| 2026-10-07 | Phases reordered: Open-Meteo is Phase 1, FCC transmitters Phase 2, ducting calculation Phase 3 | The weather pipeline can be built and tested independently of the FCC data. |
| 2026-10-07 | Tropobeat reads reference data through its own `tropobeat.elasticsearch` settings, separate from its output settings | The cluster holding reference data may differ from where events are published, and may need different credentials. |
| 2026-10-08 | Co-channel paths are discovered by a daily job: a 200-mile same-channel search per LRF, then a geodesic midpoint per transmitter | Transmitter licences change slowly, so a daily run keeps the paths current without repeating searches on every weather poll. |
| 2026-10-08 | Midpoints, distances and bearings are calculated on the WGS84 ellipsoid with tidwall/geodesic | An accurate ellipsoidal calculation from a small, permissively licensed library. |
| 2026-10-08 | The weather client accepts groups of labelled points and returns the weather grouped back, while staying unaware of FCC or LRF details | Batching and de-duplication across all paths minimises API usage, and the weather client stays independent of the discovery logic. |
| 2026-10-08 | FCC transmitter data comes from the FCC TV Query station listing, downloaded per state by `tropobeat insert fcctransmitters` from cron. Requests identify tropobeat honestly, keep certificate checks on and are paced; there is no browser impersonation | Resolves former Q2. The FCC service answers normal, identified requests, so evading its protections is neither needed nor appropriate. Running outside tropobeat keeps the slow nightly download separate from live polling. |
| 2026-10-08 | The default FCC filter is licensed full-power stations (`DTV`/`TV`/`DT`); `DTS` and `DRT` are deliberately excluded | Matches the prototype's "High Power Only" intent. See Known issues for what this leaves out. |
| 2026-10-08 | The stations and RF channels each LRF receives are taken from the demod telemetry rather than a separately maintained list | Resolves former Q1. The demods report what each LRF actually receives, so the list stays current without manual upkeep. |
| 2026-10-08 | An LRF's own stations are identified from the demod telemetry: the LRF ID, the RF channel, and the call signs in the input tag, matched to FCC records by RF channel and call sign | Resolves former Q12. The demods report no FCC facility ID, and call sign + RF channel identifies the wanted station within 200 miles. |
| 2026-10-08 | Demod telemetry is read from the daily `log-metric-p-rfdemod-YYYY.MM.DD` indices (setting `demod_index`), linked to the LRF by `s_lrf_id` | Resolves former Q4. |
| 2026-10-08 | Collecting each LRF's received channels moves from Phase 2 to Phase 1 | The 200-mile co-channel search needs each LRF's received channels as its input. |
| 2026-10-08 | Co-channel paths are kept in memory for the weather polls and published to the tropobeat index as the `cochannel` metricset, instead of a separate index | The weather poll can use the paths directly, and publishing through tropobeat's output reuses its index, template and retention while giving a dated daily snapshot. |
| 2026-10-08 | All recurring work inside tropobeat runs on a gocron scheduler | Cron and interval schedules, run at start-up, no overlapping runs, error reporting and a clean shutdown, without hand-written timing code. |
| 2026-10-13 | Tropobeat's own packages live under `internal/` | They are only meant to be used by tropobeat itself. |
| 2026-10-13 | Reading the demod channels and discovering co-channel paths run as one combined daily job | Discovery always uses the channel list from the same run, never a stale or missing one. |
| 2026-10-13 | The LRF list is re-read on every discovery run (replaces "LRF sites are loaded once at start-up", 2026-10-07) | LRFs added, moved or removed with `insert lrfcoordinates` take effect without restarting tropobeat. |
| 2026-10-13 | Own-station exclusion, refined with a day of real demod data: exclude matched call signs; exclude nothing when the tagged station isn't a full-power FCC station; skip inputs with no call sign; skip a channel whose matched own station is over 150 miles away | Excluding the "nearest" transmitter when no match is found would wrongly remove real interferers. The distance check guards against LRF code or coordinate mismatches (Q14). |
| 2026-10-13 | Co-channel path events also carry the path as a line | Kibana Maps can draw every path in one layer; it can't draw a line between two separate points in one record. |
| 2026-10-13 | The term **co-channel path** is used for an LRF ↔ same-channel transmitter link; "threat" is reserved for an active ducting event on a path | One consistent term, matching the `cochannel` metricset and field names. |
| 2026-10-13 | Weather polling defaults to hourly | Matches the forecast data's hourly resolution. The resulting API usage is tracked in Q15. |
| 2026-10-13 | The first weather poll starts when the first discovery run finishes, rather than at the same moment | Its first poll then covers the co-channel paths, instead of spending API calls on an LRF-only poll that is immediately superseded. |
| 2026-10-13 | The own station is looked up nationwide on its channel, not only within the 200-mile search, and Mexican (X) call signs in tags are recognised | Real data showed swapped LRF codes put the own station about 1,700 miles away, outside the search, where the 150-mile check could not see it. Recognising Mexican stations keeps their channels' US co-channel paths instead of skipping them. |
| 2026-10-13 | The workspace's copy of libbeat is patched to accept geographic-shape fields | libbeat's field definitions don't support the shape type needed for the co-channel path lines on Kibana maps. See Known issues. |

## 6. Known issues

- Source spreadsheet quirks are handled by tropobeat (Excel text encoding, 11 ZIP codes missing leading zeros, a leading space in one coordinate). Fixing them at the source would be cleaner.
- Elasticsearch authentication options are untested against a secured cluster (Q5).
- Development uses the free Open-Meteo API, which is for non-commercial use only. Production needs a commercial key (Q8, Q15).
- The default FCC filter excludes distributed transmission sites (`DTS`) and replacement translators (`DRT`). Some full-power stations exist only as `DTS` sites (e.g. KCWX, KVMD, KQSL), and some `DRT`s run 10–15 kW on other channels, so these transmitters are not considered unless `--services DTV,TV,DT,DTS,DRT` is used (Q11).
- The FCC station listing has no published format. If it changes, the import stops with an error rather than loading bad data. The FCC's bulk database download is the fallback source.
- Reference lookups support up to 10,000 records per set. Fine for the 211 LRFs; very large reference sets may need paging.
- Run the FCC import (cron) before the daily discovery time (e.g. FCC at 02:00, discovery at 03:00 UTC), so discovery always reads a complete, freshly refreshed transmitter list.
- Avoid `tropobeat insert … --recreate` while tropobeat is running: it briefly deletes the index. Normal re-runs are safe.
- The co-channel path line relies on a one-line patch to the workspace's copy of libbeat (allowing geographic-shape fields). Keep the patch when updating libbeat, or the build of the index template will fail.
- Demod telemetry facts from the 2026-10-13 sample: each input reports about once a minute, and about 21% of reports are `Not Locked` (ignored).

## 7. Change log

| Date | Change |
|---|---|
| 2026-10-07 | Plan created. Phase 0 complete: project set up, the `insert` command group, Elasticsearch connectivity, the LRF loader and `insert lrfcoordinates`. |
| 2026-10-07 | Phase 0 committed, including the LRF spreadsheet and this plan. Q6 resolved. |
| 2026-10-07 | Weather source set to Open-Meteo. Phases reordered (Phase 1 Open-Meteo, Phase 2 FCC transmitters, new Phase 3 ducting calculation; later phases renumbered 4–8). Weather client and its settings built. Q3 resolved; Q8–Q10 added. |
| 2026-10-07 | Repository history cleaned up before the first push. Tropobeat now connects to Elasticsearch, reads all LRFs at start-up, and creates the weather client. |
| 2026-10-08 | Phase 2 redefined around a daily co-channel discovery job (200-mile same-channel search, geodesic midpoints, grouped path points). Grouped weather requests added. Q11 added. |
| 2026-10-08 | Midpoints set to an accurate ellipsoidal (WGS84) calculation using tidwall/geodesic. |
| 2026-10-08 | Built `tropobeat insert fcctransmitters` (cron job) and the `fcc-transmitters` index. The insert commands can take Elasticsearch credentials from environment variables. Q2 resolved; Q11 narrowed. |
| 2026-10-08 | Q1 resolved: each LRF's received stations and RF channels come from the demod telemetry. Q12 added. |
| 2026-10-08 | Q12 and Q4 resolved: own stations matched by RF channel + call sign; demod telemetry read from the daily `log-metric-p-rfdemod` indices. Received-channel collection moved to Phase 1. Q13 added. |
| 2026-10-08 | Co-channel paths kept in memory and published as the `cochannel` metricset instead of a separate index. All recurring work set to run on a gocron scheduler. |
| 2026-10-13 | Tropobeat's packages moved under `internal/`. Plan rewritten as a customer-facing document. A day of real demod telemetry analysed: own-station exclusion refined, Q13 answered and narrowed, Q14 (LRF code mismatches) and Q15 (API budget) added. Planned: `lrf_sites` to limit runs to chosen LRFs, optional merging of nearby weather points, hourly weather polling, and co-channel paths drawn as lines on maps. |
| 2026-10-13 | Built Phase 1 and Phase 2 core: the gocron scheduler; the combined daily job that reads each LRF's received channels from the demod telemetry and discovers co-channel paths (own-station exclusion, nationwide own-station check, geodesic midpoints); co-channel paths kept in memory and published as the `cochannel` metricset with path lines for Kibana Maps; the hourly weather poll over every path point, published as the `weather` metricset; `lrf_sites` and `coordinate_precision`. Verified end to end against Elasticsearch with a day of real demod telemetry and all 1,749 FCC full-power transmitters. Q13–Q15 updated with measured figures. |
| 2026-10-13 | Architecture diagram redrawn for readability. Added `openmeteo.enabled` to turn weather polling off and run co-channel discovery only. |
