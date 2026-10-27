# Tropobeat Rolling Plan

**Automated Tropospheric Ducting & Telemetry Correlation Engine**

| | |
|---|---|
| Last updated | 2026-10-27 |
| Current phase | Phase 4b (correlation engine) built; Phase 4c (alerts and inSITE integration) next |
| Platform | Evertz Magnum Analytics (inSITE), Elasticsearch, RabbitMQ, Open-Meteo API |

This is a living document. Update it at the end of each work session: move finished items to **Done**, add new findings to **Open questions** and **Decisions**, and add a line to the **Change log**. When a phase (or a user-visible feature) is completed, also update [README.md](README.md), which describes how tropobeat currently works. When a correlation rule, threshold default or verdict changes, update [CORRELATION.md](CORRELATION.md) too. Question numbers are never reused, so gaps mean a question was resolved (see **Decisions**).

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
       │  poll               │                                        ┌┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┐
       └──────────┬──────────┘                                        ┆  Demod pollers        ┆
                  │  weather per path point                           ┆  (outside tropobeat)  ┆
                  ▼                                                   └┄┄┄┄┄┄┄┄┄┄┄┬┄┄┄┄┄┄┄┄┄┄┄┘
       ┌─────────────────────┐             ┌─────────────────────────────┐        ▼
       │  Ducting            │             │  Live demod reports         │◄── RabbitMQ
       │  calculation        │             │  (MER, Pre-FEC BER, power,  │    exchange
       └──────────┬──────────┘             │  lock), ~1/min per input    │
                  │                        └──────────────┬──────────────┘
                  │                                       │
       ┌──────────┴──────────┐             ┌──────────────┴──────────────┐
       │  Latest ducting     │             │  7-day baselines per demod  │◄── daily, from
       │  per path (memory)  │             │  input (memory, published)  │    demod telemetry
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
- **Ducting results** are published on every weather poll in two forms: a short summary on each `weather` event (a score and class per point, and a ducting index per path), and the full detail for each weather location as the `ducting` metricset. [DUCTING.md](DUCTING.md) explains the classes, the scoring and every field.
- **Live RF telemetry** arrives from a RabbitMQ exchange, about once a minute per demod input, into tropobeat's own bounded queue. Each input is judged against its own **baseline**: the 7-day median Pre-FEC BER and MER, computed daily from the demod telemetry in Elasticsearch, held in memory and published as the `baseline` metricset.
- **Correlation** runs inside tropobeat as the live reports arrive. Each LRF and channel gets one verdict, published through tropobeat's normal output as the `correlation` metricset, with a searchable decision trace (`correlation_trace`). [CORRELATION.md](CORRELATION.md) describes the rules. Alerting follows in Phase 4c.
- **Locations** are stored as geographic points (and co-channel paths as lines), so distance queries and Kibana maps work directly.

## 3. Phases

### Phase 0: Foundation ✅

| Item | Status | Notes |
|---|---|---|
| Project set up as an Elastic Beat named tropobeat | ✅ | Based on the standard beat template, renamed from its placeholder name. |
| Data-loading commands: `tropobeat insert …` | ✅ | A command group for loading reference data, separate from the running beat. |
| Elasticsearch connectivity | ✅ | Uses the official Elasticsearch Go client. Connection options cover hosts, username/password or API key, a custom CA certificate, and (for testing only) skipping certificate checks. |
| LRF list loader | ✅ | Reads the LRF spreadsheet export. Copes with its quirks (Excel text encoding, ZIP codes missing leading zeros, stray spaces) and validates every coordinate. |
| `tropobeat insert lrfcoordinates` | ✅ | Loads the 211 LRFs into the `lrf-coordinates` index with each site's location as a geographic point. Re-running updates sites rather than duplicating them. Supports a preview mode (`--dry-run`) and rebuilding the index (`--recreate`). Verified against Elasticsearch 8. The index is created in lookup mode, so dashboards can join it to label LRF codes with their city and state, for example "BOS · Andover, MA". |
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
| Weather poll | ✅ | Fetches weather for every co-channel path point (LRF, midpoint, transmitter) and for LRFs with no paths, and publishes one `weather` event per path or LRF. Each event is timestamped when the weather was fetched and records the hour the weather applies to separately. Path events also carry the path as a line, so weather can be shown on the same Kibana map as the co-channel paths. Interval set by `period` (default 1 hour). If discovery fails, it polls the LRFs only. Each run logs how many points were requested, how many unique locations were fetched and how many requests were sent. Verified with a limited run against the live Open-Meteo API (3 LRFs: 49 paths, 147 points, 80 unique locations, 2 requests). |
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

### Phase 3: Tropospheric ducting calculation (`internal/ducting`) 🔄

Turns the weather at each path point into refractivity, layer gradients, ducts and a ducting likelihood, and combines a path's three points into one ducting index. Runs on every weather poll. **[DUCTING.md](DUCTING.md)** is the technical reference: what each class means, how scores are calculated, and what every field in the results means.

| Item | Status | Notes |
|---|---|---|
| Above-ground profile | ✅ | Each location's profile is the 2 m surface point plus the pressure levels above it. Levels the weather model extrapolates below the terrain are dropped (at Denver, 1597 m, six of eight levels), as are levels less than 20 m above the point below, which would give noisy gradients. The number dropped is recorded. |
| Refractivity N and modified refractivity M | ✅ | Water vapour pressure from temperature and humidity (Magnus formula), refractivity N = 77.6/T · (P + 4810·e/T), and modified refractivity M = N + 0.157·h. These are the formulas ITU-R P.453 gives for radio work; checked against the recommendation's full forms, they agree to within 1.2 N-units across −40 to 35 °C, all humidities and 700–1050 hPa. |
| Layer gradients and classes | ✅ | The gradient of N and M through each layer, classed per ITU-R P.453: sub-refractive (above 0 N/km), normal (0 to −79), super-refractive (−79 to −157) or ducting (below −157, where M decreases with height). A location takes the class of its steepest layer. |
| Duct properties | ✅ | Each run of ducting layers is one duct: surface, surface-based or elevated, with its base, top and thickness above ground and its strength (the fall in M across it). |
| Ducting likelihood score | ✅ | 0 to 1 from the steepest gradient: 0 for normal air, rising to 0.6 at the ducting boundary and to 1 at −300 N/km. A likelihood rather than a yes/no, because the model's 200–1,000 m level spacing dilutes thin ducts. The bands are settings, with initial values to be calibrated (Q16). |
| Frequency-dependent trapping | ✅ | Whether a duct is thick enough to trap the path's channel: the lowest trapped frequency is 360,000 / thickness^1.5 MHz (thickness in metres), from the US Naval Postgraduate School's propagation course. Low VHF needs much thicker ducts: about 340 m for RF 2, 160 m for RF 7 and 85 m for RF 14. |
| Path ducting index | ✅ | The plain mean of the scores at the LRF, midpoint and transmitter, plus a separate yes/no for whether any of the three points has a duct that traps the path's channel. A refinement is open (Q18). |
| Published results | ✅ | A summary on every `weather` event (each point's score, class and steepest gradient; each path's index, channel frequency and trapping flag), and a `ducting` metricset with one event per unique weather location per poll: every height's refractivity, every layer's gradient and class, the ducts found, and the paths and LRFs that use the location, so maps and searches can move between paths and their ducting detail. The `ducting.publish` setting chooses every location (`all`, the default during calibration) or only super-refractive and ducting ones (`notable`) (Q19). |
| Technical readme | ✅ | [DUCTING.md](DUCTING.md): the calculation, classes, duct types, scoring, a field reference, how to read results, and setting up Kibana Maps. |
| Calibration against radiosonde soundings | 📋 | Compare results from model levels with full-resolution weather-balloon soundings near LRFs on known ducting days, to measure how often coarse levels detect, weaken or miss ducts, and to set the score bands (Q16, Q17). |
| Forecast hours | 📋 | Scoring forecast hours ahead is part of Phase 6. |

### Phase 4: RF telemetry integration 🔄

Brings in the live RF side: every demod input's MER, Pre-FEC BER, power and lock status, about once a minute, judged against what is normal for that input and correlated with the ducting results. Split into three parts: **4a** the foundation (live stream, baselines, ducting held in memory), **4b** the correlation engine (the detection rules), and **4c** the output (events and alerts).

**Demod telemetry.** Stored in inSITE in the daily `log-metric-p-rfdemod-YYYY.MM.DD` indices, and sent live on a RabbitMQ exchange in the same form. Fields under `rfdemod.rf_demod`: `i_input_mer` (MER, whole dB), `d_pre_fec_ber` (Pre-FEC BER), `i_input_power` (input power, dBm), `s_input_status` (lock status), `i_rf_channel`, `s_input_tag`, `s_lrf_id` (LRF code), `s_demod_side` (A or B), and the card identity `s_card_label`, `s_frame_label`, `i_slot`, `i_input`; plus `host.ip`, the demod frame's address. `as_ids` (the collector's internal polling IDs) is not used.

**Demod instance.** One demod input is identified by its frame's host IP, card slot and input number. LRF, side and channel are properties of the input, not its identity. Measured over 7 days of test data: 2,818 inputs on 413 frames; nearly every LRF has two frames, one per side (A and B). Identifying inputs this way keeps apart the 11 channels that are received twice on the same frame, which an LRF-side-channel key would merge.

#### Phase 4a: foundation ✅

| Item | Status | Notes |
|---|---|---|
| Live demod stream from RabbitMQ | ✅ | Tropobeat declares its own queue, bound to the demod exchange, so it receives a copy of every report without taking them from other consumers. By default it only checks that the exchange exists and never changes it, since the exchange belongs to inSITE. The queue is bounded: reports older than 5 minutes expire and beyond 50,000 the oldest are dropped, so a stopped tropobeat can't build up a backlog of stale reports. The broker sends at most 500 unacknowledged reports at a time, and each is acknowledged once received. Malformed or incomplete reports are rejected. TLS is supported, and the password is required, kept in the keystore and never logged. Off by default. Settings follow the routelogbeat naming. |
| Reconnects | ✅ | One supervisor owns the connection, so any failure (a dropped connection, a closed channel, a broker restart, a deleted queue) leads to a fresh connection. The wait doubles from 1 second up to 30 seconds, with some randomness, and resets after a minute connected. The stream to the rest of tropobeat stays open throughout. Improves on the routelogbeat client, which could stay stuck when only the channel closed. Verified against a local broker by forcing each case during a replay. |
| Ingest summary | ✅ | Every minute: reports decoded (and how many locked), demod inputs and LRFs seen, reports rejected, and reconnects. Verified with the 2026-10-13 sample replayed: 2,815 inputs at 207 LRFs; the only rejected reports were the sample's few without an LRF ID. |
| Daily baselines | ✅ | For every demod input and RF channel, the **median Pre-FEC BER** and **median MER** of its locked reports over the last 7 whole days (up to the start of today, so the result is the same all day). Runs at start-up and daily at 03:30 UTC, after discovery. The BER median uses Elasticsearch's HDR method, which matched the exact median on every test input; the default method was more than 10% out on a third of them. The MER median is computed exactly. The full network takes about 10 to 16 seconds. |
| BER floor | ✅ | 96% of locked reports have a BER of exactly 0, so most medians are 0. A spike is measured against an **effective BER** of at least 1e-6 (setting). |
| Baseline status | ✅ | `ok`; `insufficient` (under a day of reports); `invalid` (a saturated median of 16.777215, as spare inputs that report "Locked" do); or `no_station` (the tag names no station). Only `ok` baselines will be used to judge live telemetry. A warning is logged if the demod data doesn't reach back over the whole window. |
| `baseline` metricset | ✅ | One event per demod input and channel on every run: the input, LRF and its location, side, channel, card and frame, tags and call signs, status, sample count and time span, BER median and effective BER, and MER median. A published record for dashboards and evidence; tropobeat always recomputes baselines from the demod telemetry and never reads them back. Verified on the test cluster: TERH side A, RF 10: 9,884 reports, BER median 0 (effective 1e-6), MER median 33 dB. |
| Baselines and ducting in memory | ✅ | Both are replaced as a whole on each refresh (baselines daily, ducting on every weather poll), so reads never wait and always see one complete set; a failed refresh keeps the previous set. Baselines older than 48 hours (two missed runs) and ducting older than two weather periods are treated as missing, so stale data can never confirm interference. Each co-channel path's ducting result is indexed by LRF and channel, so a demod report leads straight to "does any co-channel path on this channel have dM/dh < 0?". |
| Replay tool for testing | ✅ | Publishes the demod sample to a RabbitMQ exchange, in time order, at real or accelerated speed, optionally moved to the current time. Lets the stream be tested without inSITE. |

#### Phase 4b: correlation engine ✅

A correlation engine inside tropobeat that evaluates each demod report as it arrives. One worker owns all live state (the latest reading and the counters of every demod input), so no locking is needed; a sweep every 10 seconds handles inputs that go quiet, verdicts that are waiting, ducting that changes and periodic re-publishing. **[CORRELATION.md](CORRELATION.md)** describes the rules for operators and engineers: the evidence, each step, the verdicts, the decision trace, settings and fields.

| Item | Status | Notes |
|---|---|---|
| BER spike, MER drop and lock loss | ✅ | Per demod input, against its own baseline. **BER spike:** locked, and BER at least 10× the effective baseline, or a saturated reading. **MER drop:** locked, and MER at least 3 dB below the baseline median. **Lock loss:** a normally locked input (one with a usable baseline) reports not locked; strong interference drives a channel to "No Signal". Thresholds are settings (`ber_factor`, `mer_drop_db`, `lock_loss`). Inputs without a usable baseline are not judged, and are visible in the decision trace. |
| Environmentally Stressed | ✅ | A BER spike that holds for 3 consecutive reports while the input stays locked. Recorded with the input's evidence. |
| 3-report hysteresis | ✅ | An input becomes degraded after 3 consecutive anomalous reports (about 3 minutes) and clears after 3 clean ones. A missed report counts neither way; a gap of more than 3 minutes starts the counts again. |
| A/B pairing | ✅ | The side A and B inputs of each LRF and channel are paired as their reports arrive. A side counts only with a report in the last 90 seconds; for a channel received twice on one frame, the side is degraded if either input is. Inputs with no side label are judged but not paired (Q23). A one-sided verdict waits 2 minutes (`pair_grace`), so the other side's own 3-report count can catch up at the start and end of an A+B episode. |
| Ducting confirmation | ✅ | Both sides degraded **and** a co-channel path on that channel at `ducting` or `trapping` (dM/dh < 0) in the latest weather poll: confirmed co-channel interference. Trapping (a duct thick enough for the channel) is reported as the stronger evidence, and the strongest path's transmitter (the nearest, on a tie) is named as the likely interferer. Ducting older than two polls never confirms. Paths with no usable weather make the ducting unknown rather than absent. |
| Verdicts | ✅ | Each LRF and channel has one verdict: `clear`, `single_side` (one demod: equipment, not CCI), `unpaired` (the other side can't be judged), `dual_ducting_unknown`, `dual_unexplained` (signal degraded, no path ducting) or `cci_confirmed`, ordered by severity 0 to 5. |
| Lock-loss guard | ✅ | Lock loss counts only when another input on the same demod frame is still locked, so a frame or site going dark is never read as interference. A stand-in until the outage and frame rules below. |
| Interference signature | ✅ | At onset, a degraded input's power is compared with its level just before: `steady_or_rising` (typical of interference) or `dropping` (a fade, the feed or the transmitter). Recorded as supporting evidence; never decides a verdict. |
| `correlation` metricset | ✅ | Published when a verdict starts, changes (including stronger or weaker ducting evidence), every 5 minutes while it lasts (`snapshot_interval`), and when it clears, with the episode's duration and peak verdict. Each event carries both sides' inputs, readings, the baseline values and thresholds they were judged against, the signature, the ducting evidence and the LRF's location, plus the shared filter fields. The `baseline` metricset now also carries each input's thresholds. |
| Decision trace | ✅ | The `correlation_trace` metricset records how each verdict was reached, with a reason code and a one-line explanation, in categories that can be turned on separately: `pair` and `hysteresis` (on by default), `ducting`, `skip`, `input` (every report; limit with `trace.lrf_sites`) and `sweep`. Trace records can also go to the log. If they arrive faster than they can be published the excess is dropped and counted; verdicts never are. |
| Correlation debug dashboard | ✅ | **[Tropobeat] Correlation debug** ships with tropobeat: the latest verdict per LRF and channel, verdicts over time, trace reasons over time, each pair decision with its explanation, MER and BER against their thresholds, and every trace record. See [DASHBOARD.md](DASHBOARD.md). |
| Test scenarios and demo mode | ✅ | The replay tool injects conditions (BER spikes, saturated BER, MER drops, fades, lock loss, dropped reports, missing side labels, flapping) on chosen LRFs, channels and sides at set times, and can replay endlessly for unattended demos, reconnecting by itself. Mock weather can now be set per LRF (including a no-data case) and made to fail on a schedule. A shipped scenario reproduces every verdict in about 45 minutes; a second is a 60-minute demonstration cycle. |
| Outage exclusion | 📋 | Deferred from 4b. Every expected channel at an LRF (inputs with an `ok` baseline, both sides) loses lock or collapses in power and MER, with no dM/dh < 0 on any of the LRF's co-channel paths: flagged as a local power or backhaul outage, and environmental alerts for that LRF are suppressed (Q22, Q25). |
| Frame loss | 📋 | Deferred from 4b. Every input on one frame loses lock while the other side is fine: reported as an equipment or frame fault, not environmental. The lock-loss guard already keeps such cases from being read as interference. |

#### Phase 4c: output 📋

| Item | Status | Notes |
|---|---|---|
| Events and alerts | 📋 | Verdict events (start, update, ongoing, clear) are published since 4b. Still to decide: alert integration with inSITE (which verdicts alert, and to whom), periodic network-wide state snapshots, and outage and frame-fault events. |

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
| Mock weather for dashboard building | ✅ | Tropobeat can run with simulated weather instead of Open-Meteo (`openmeteo.mock`), while everything else stays real. The simulated weather is built so the ducting calculation produces each state on purpose: normal, super-refractive, a thin duct that traps nothing, an elevated duct, a duct that traps UHF only, and a thick duct that traps VHF too. A mixed mode moves every site through all states on a set cycle, varying with place so different sites differ at the same time. Simulated events are tagged `mock-weather`. No Open-Meteo calls are made. |
| Shared fields across all results | ✅ | Every published result (co-channel paths, weather, ducting and baselines) now carries the same identifying fields: LRF, co-channel path, RF channel, TV band (low VHF, high VHF, UHF), the co-channel transmitters (call sign and FCC facility ID), the LRF's own stations, and the discovery run. Weather and ducting results also share the forecast hour. One dashboard filter (an LRF, a channel, a band, a station) therefore works on every panel and map layer at once, and a single path ID leads from a map line to its weather, its ducting detail and the demod inputs it could interfere with. Each baseline lists the co-channel paths on its LRF and channel: the transmitters that could interfere with that input. The fields replace the per-result fields they duplicated. Verified on the test cluster. |
| Ducting state | ✅ | A four-step summary on every weather and ducting result: normal, super-refractive, ducting (a ducting layer, but no duct thick enough to trap the channel) and trapping (a duct traps the channel), also given as a level from 0 to 3 so dashboards can show the highest level over an hour or across an LRF's paths. Unlike the path ducting index, one strong point is enough to raise it. Rules in [DUCTING.md](DUCTING.md#ducting-state); the thresholds follow the score bands and will move with their calibration (Q16, Q18). |
| Ducting monitoring dashboard | ✅ | **Ducting Watch**, a Kibana dashboard that ships with tropobeat and loads with its standard dashboard setup (or by importing one file). LRF and band selectors and a time slider drive every panel. Headline figures: LRFs with a trapping duct, paths ducting now, the selected LRF's state, and the worst path with its state, index and distance. A map of the co-channel paths that are ducting or trapping at the latest poll, with their midpoints and transmitters (click a path to filter the dashboard to it). A table of every path at the latest poll, with its state and the score at the LRF, midpoint and transmitter colour-coded. Heat maps of the network (LRFs against time, each labelled with its site's city and state) and of each co-channel path, fixed to the last 72 hours; a click on the path heat map picks the path. For a picked path, its three scores over time and the duct detail at each point, labelled LRF, midpoint or transmitter. A reference panel recalls the states and thresholds. Built and checked on the test cluster with mock weather, and installed into a clean Kibana space both ways. See [DASHBOARD.md](DASHBOARD.md). MER and BER on the same time axis are next, now that the correlation engine publishes them; a forecast extension past "now" follows in Phase 6. |
| Map of LRFs, transmitters and co-channel paths | 📋 | The Ducting Watch map draws only the co-channel paths that are ducting or trapping at the latest poll, with their midpoints and transmitters, coloured and sized by the path ducting index; every LRF is shown. Still to do: colour by ducting state with trapping paths emphasised, and LRFs coloured by their highest current state. |
| Site labels on dashboards | ✅ | The readable transmitter label (e.g. "WVER site 2 of 4 (5 kW)") replaces the bare call sign in the Ducting Watch path table and "worst path" tile, the map tooltips of both dashboards, Correlation debug and the CCI operations preview (cards, likely-interferer path and station), so the sites of a multi-site station can be told apart. Single-site stations still show their call sign, and results written before the labels existed fall back to it. Map markers keep the short call sign. The 7-day top-interferers chart stays ranked by station. Checked on the test cluster; hand-built dashboards there received drafts for review. |
| Co-channel verification dashboard | ✅ | **Co-channel paths**, a second dashboard that ships with tropobeat, to check that discovery has run. One map of the latest discovery run, with an LRF picker: every co-channel path coloured by band, its midpoint and transmitter, and every LRF labelled with its code, city and state, as on the Ducting Watch heat map. Hovering a path shows its LRF, transmitter, facility ID, channel, band, distance, bearing, ERP, service, the LRF's own stations and the discovery run; hovering an LRF shows how many paths and channels it has. An LRF without paths stands out as a plain grey square. See [DASHBOARD.md](DASHBOARD.md). |
| Event timeline and correlation dashboard | 📋 | Telemetry overlaid with the ducting index for each event. The Correlation debug dashboard (Phase 4b) covers checking the engine; this is the operator-facing view. |

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
| Q10 | Which weather model: `best_match` (default) or HRRR for CONUS? Confirm coverage for the Alaska and Puerto Rico LRFs. Also confirm the chosen models report humidity relative to water at all temperatures, as the ducting calculation assumes. | Phase 1 config, Phase 3 accuracy |
| Q11 | Which FCC records count as an "active" transmitter beyond the default (licensed full-power `DTV`/`TV`/`DT` and distributed-transmission `DTS` sites, PR included)? Should construction permits, Class A (`DCA`), LPTV (`LPD`) or replacement translators (`DRT`) be added? All can be enabled with `--services`/`--statuses`. Measured 2026-10-26: adding Class A (`DCA`) brings in 403 licensed transmitters and 630 more co-channel paths across the network (1,817 → 2,447), and lets 32 LRF channels whose own station is Class A exclude it properly. See Q15 for the API cost. | Phase 2 discovery filter |
| Q13 | Should demod input tags be standardised at source? Real data (2026-10-13), 1,095 locked channels at known LRFs: 962 (about 88%) have tags matching a full-power FCC station by call sign; 118 (about 11%) name LPTV, Class A, translator or Mexican stations (handled: nothing excluded); 4 are spare, test or empty inputs (skipped); 11 belong to the swapped MON/MONR codes (Q14). The tags vary in spacing and `(OTA)` style (`KOLR (OTA)` vs `KOLR ( OTA )`, `WMTW(OTA)`), which tropobeat tolerates but which a naming standard would make more reliable. | Phase 2 own-station exclusion accuracy |
| Q14 | Seven LRFs use different codes in the demod telemetry than in `lrf-coordinates`: `BIRM`/`BIR`, `HTSBG`/`HSTBG`, `LUB`/`LUBB`, `PRESQ`/`PRSQ`, `RNO`/`RNC`, and `MON`/`MONR` are swapped (in the LRF list `MON` is Monroe LA; in the demods `MON` is Monterey CA). Which codes are authoritative: fix the LRF list, fix the demod labels, or add a translation table in tropobeat? Until resolved, these sites are skipped by co-channel discovery: the five mismatched codes as unknown LRF IDs, and the MON/MONR channels by the 150-mile own-station check (verified). Also, `GLEN`, `GRNNC`, `VICT` and `WATER` had no demod data in the 2026-10-13 sample. | Phase 2 discovery for those 11 sites |
| Q15 | Open-Meteo API budget. Open-Meteo counts calls per location, and a request with more than 10 weather variables counts as several calls. Measured on the full network (2026-10-13): 1,829 co-channel paths give 5,512 path points, of which 2,589 are unique locations; at 27 variables each poll costs about 7,000 calls. Hourly polling is about 5.0 million calls a month, right at the Professional plan's 5 million limit (with `coordinate_precision` 0.01: about 4.6 million; 0.05: about 4.2 million; every 3 hours: about 1.7 million; every 6 hours: about 0.8 million). Which plan will be purchased, and should usage be reduced by polling less often (one poll's hourly forecast covers the hours in between), requesting fewer variables or pressure levels, or merging nearby points (`coordinate_precision`)? During trials, `lrf_sites` limits usage to chosen LRFs, and `openmeteo.enabled: false` turns weather polling off entirely while co-channel discovery keeps running. Remeasured 2026-10-26 (27 variables, about 2.7 calls per location), unique weather locations per poll and hourly cost per month: full-power only 2,576 locations, about 5.0 million (4.6 million with `coordinate_precision` 0.01; 4.1 million with 0.05); with Class A 3,303 locations, about 6.4 million (5.9 million; 5.3 million). Every 3 hours: 1.7 million without Class A, 2.1 million with. Every 6 hours: 0.8 million without, 1.1 million with. | Production weather polling |
| Q16 | Ducting score calibration. The score bands (−79, −157 and −300 N/km, and 0.6 at the ducting boundary) are initial values taken from the ITU-R P.453 classes. Which known ducting events and weather-balloon soundings should they be calibrated against, and who signs off the final values? Links to Q7. | Phase 5 detection rules |
| Q17 | Vertical resolution. The weather model's pressure levels are 200–1,000 m apart, while real ducts are often 20–200 m thick, so thin ducts are under-scored or missed (a thin strong duct inside a 200 m layer can read as only super-refractive). Is more weather detail near the ground (more levels or a higher-resolution model) worth the extra API usage? Links to Q10, Q15. | Phase 3 accuracy |
| Q18 | Path ducting index method. The index is a plain mean of the three points' scores, so it ignores the channel frequency (a duct too thin for a VHF channel still raises that path's index) and where along the path the duct sits (one strong point and two normal ones score the same as three moderate ones). Should Phase 5 move to a frequency-aware or midpoint-weighted index? | Phase 5 scoring |
| Q19 | Ducting data volume. With `ducting.publish: all`, every weather location gets a ducting event on every poll: about 62,000 documents a day on the full network at hourly polling. When should production switch to `notable` (super-refractive and ducting locations only), and what retention suits the ducting data? Links to Q15. | Phase 8 deployment |
| Q20 | RabbitMQ demod stream: tropobeat defaults to the exchange `x.tropobeat.demod` with routing key `demod.metrics`. Will inSITE publish the demod reports there (and with which exchange type), who provides the account and password, and is TLS required? | Phase 4a in production |
| Q21 | Do the production demod indices keep at least 7 days? The baselines need 7 whole days; tropobeat warns when the data is shorter. | Full baselines |
| Q22 | What should happen when every channel at an LRF is lost **and** a co-channel path shows dM/dh < 0? The rules cover only the case without ducting (an outage). | Phase 4b outage rule |
| Q23 | Should the demod side labels be cleaned up at source? Some inputs report `1 A` instead of `A` (handled), and a few report no side. | Phase 4b A/B pairing |
| Q24 | How should LRFs that are down for long periods be shown? Six LRFs were entirely unlocked in the 2026-10-13 sample; with no locked reports they get no baseline and are ignored by the rules. | Phase 4b/4c |
| Q25 | Should a demod input (or a whole LRF) that stops reporting altogether count as an outage? | Phase 4b |
| Q26 | Threshold calibration and flapping channels. The test data is one real day (2026-10-13) copied across 12 days, so it shows how the rules behave on that day but can't tell a channel that is marginal every day from one that had a single bad day. On that day, without the ducting check, the BER rule would raise 1,749 three-minute episodes on 145 inputs (57 with 10 or more), and the MER rule 1,115 on 203. Real multi-day data is needed before tuning thresholds or adding special handling. Links to Q16. | Phase 4b tuning |
| Q28 | Correlation timing settings. A one-sided verdict waits 2 minutes for the other side, a side counts with a report in the last 90 seconds, and an episode is re-published every 5 minutes. These were chosen for once-a-minute reports. Are they right for operations, and should the re-publish interval differ for confirmed interference (for example every minute) and for other verdicts? | Phase 4c alerting |
| Q29 | Which verdicts should alert in inSITE, and with what wording? `cci_confirmed` clearly should; should `dual_unexplained` (signal degraded, no ducting) alert as a non-environmental fault, and should `single_side` raise an equipment alert? | Phase 4c |
| Q27 | Formal conformance with ITU-R P.453. The ducting calculation follows ITU-R P.453 for refractivity, modified refractivity and the duct condition, but works out water vapour pressure with the widely used Magnus formula rather than the recommendation's own formula (which adds a small pressure correction). The two agree to within about 1.2 N-units, too little to change any class, duct or score. Should the calculation switch to the recommendation's vapour-pressure formula (over water at all temperatures), so results can be described as following ITU-R P.453 in full? Links to Q10. | Reporting and documentation; no effect on results |

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
| 2026-10-14 | Weather events are timestamped when the weather was fetched; the hour the weather applies to is kept in its own field | Dashboards show when data arrived, while the forecast hour stays available for analysis. |
| 2026-10-14 | Weather events for a co-channel path also carry the path as a line | Weather can be drawn on Kibana maps along each path, in the same way as the co-channel paths. |
| 2026-10-14 | The ducting calculation uses the Magnus vapour-pressure formula and the ITU-R P.453 radio refractivity formula, with the recommendation's full forms used only as a cross-check | The simpler formulas are standard for radio work and agree with the full forms to within 1.2 N-units, far smaller than the gradients that matter (16 N-units or more per 100 m). The accuracy limit is the weather model's vertical resolution, not the formula (Q17). |
| 2026-10-14 | Ducting results are published as a summary on `weather` events and in full as a separate `ducting` metricset, one event per unique weather location per poll | Dashboards and alerts use the short summary; the full profile stays available for analysis without making every weather event large. An LRF shared by many paths is analysed and stored once. |
| 2026-10-14 | The path ducting index is the plain mean of the three points' scores, with duct trapping for the channel reported separately | Simple and easy to explain while the scoring is calibrated; refinements are tracked in Q18. |
| 2026-10-14 | Duct trapping uses the lowest trapped frequency 360,000 / thickness^1.5 MHz, from the US Naval Postgraduate School's propagation course | A published rule that depends only on duct thickness. An alternative that also uses duct strength could not be confirmed from a source, and a published comparison of trapping methods questions that approach. |
| 2026-10-14 | A ducting event is published for every location by default (`ducting.publish: all`), with an option to publish only super-refractive and ducting locations | Normal-day profiles are needed as the baseline for calibration (Q16) and Phase 4; production volume is tracked in Q19. |
| 2026-10-14 | Ducting events list the paths and LRFs that used their location | One search or map filter moves from a co-channel path to its three ducting results. |
| 2026-10-13 | The workspace's copy of libbeat is patched to accept geographic-shape fields | libbeat's field definitions don't support the shape type needed for the co-channel path lines on Kibana maps. See Known issues. |
| 2026-10-19 | Phase 4 is split into 4a (foundation: live stream, baselines, ducting held in memory), 4b (correlation engine) and 4c (output) | Builds and verifies the data path before the rules that depend on it. |
| 2026-10-19 | Live demod reports come from a RabbitMQ exchange, into tropobeat's own bounded queue; by default tropobeat only checks the exchange exists | The exchange belongs to inSITE; an own queue gets a copy of every report without disturbing other consumers; the bounds stop a stopped tropobeat from building a backlog of stale reports. |
| 2026-10-19 | The RabbitMQ client is based on routelogbeat's, reworked around one supervisor and with the same setting names | The routelogbeat client could stay stuck when only its channel closed; one supervisor makes every failure lead to a fresh connection. Shared setting names keep the two beats familiar to operators. |
| 2026-10-19 | A demod input is identified by its frame's host IP, card slot and input number | Given by inSITE. Keeps apart inputs that an LRF-side-channel key would merge (11 channels are received twice on one frame). |
| 2026-10-19 | Baselines are the 7-day median Pre-FEC BER and MER per demod input and channel, over whole UTC days up to the start of today, recomputed daily | A median is unaffected by short spikes; whole days give the same answer all day and never include future-dated data; the median barely moves in a day, so hourly runs would add load without benefit. |
| 2026-10-19 | The BER median uses Elasticsearch's HDR percentiles; the MER median is computed exactly | The default method interpolates between BER's 1e-6 steps and was more than 10% out on a third of the test inputs; HDR matched the exact median on all of them at the same speed. |
| 2026-10-19 | A spike is measured against an effective BER of at least 1e-6 | Most inputs report a BER of exactly 0, which would make any non-zero BER a "10×" spike. |
| 2026-10-19 | MER drop = 3 dB or more below the baseline median | The pasted rules define the BER spike but not the MER drop. |
| 2026-10-19 | Baselines are published daily as the `baseline` metricset, as a record; tropobeat always recomputes them and never reads them back | Dashboards and alerts can show the baseline a reading was judged against, without tropobeat depending on its own output index. |
| 2026-10-19 | Baselines and ducting results are held in memory as complete sets replaced on each refresh, with no cache library | They are replaced in full daily and hourly, so per-entry expiry adds nothing; whole-set replacement gives lock-free reads and keeps the previous set when a refresh fails. Cache libraries (ttlcache, otter, ristretto, gocache) were considered and found unnecessary. |
| 2026-10-19 | The A and B demods share one antenna feed | Given by inSITE. A+B agreement rules out a single-demod fault, but is not independent path evidence; dM/dh < 0 on a co-channel path is the environmental evidence. |
| 2026-10-19 | dM/dh < 0 at a path point means its ducting class (a layer where modified refractivity falls with height), from the latest weather poll | The ducting calculation already classes every layer by its gradient; ducting results are hourly while telemetry is per minute, so each minute uses the latest poll. |
| 2026-10-19 | The correlation engine evaluates reports as they arrive, in one worker that owns the live state (Phase 4b) | Correlation keeps up with the data, and a single owner needs no locking. |
| 2026-10-20 | The identifying fields (LRF, path, channel, band, transmitters, own stations, discovery run, forecast hour) are the same on every result, always as lists, and replace the per-result fields | A dashboard filter applies to one field name, so different names per result left panels and map layers out of a filter. One set of names, and no duplicates, keeps the results small and unambiguous. |
| 2026-10-20 | The co-channel transmitters and the LRF's own stations are kept in separate fields | One combined field would mix "this station is received here" with "this station could interfere here". |
| 2026-10-20 | The discovery run and a baseline's own run are kept as separate fields | The two runs happen at different times; one shared name would mix them. |
| 2026-10-20 | Tropobeat publishes a ducting state (normal, super-refractive, ducting, trapping), rather than leaving it to each dashboard to work out | The map, the heat maps and future alerts then use one rule, which changes in one place when the thresholds are calibrated. |
| 2026-10-20 | At start-up, the first baseline run waits for the first discovery run, as the first weather poll does | Baselines then list their co-channel paths from the outset; discovery takes seconds, so the delay doesn't matter for live telemetry. |
| 2026-10-20 | The ducting dashboard is built in Kibana, the platform inSITE dashboards run on, rather than as a separate web application | Tropobeat already publishes everything the dashboard needs, with shared filter fields, so standard Kibana panels cover the design. Nothing extra to host, secure or keep in step with tropobeat. |
| 2026-10-20 | The dashboard's heat maps and the "where on the path" chart always show the last 72 hours, independent of the dashboard time and slider | They give the trend while the slider picks the moment for the figures, map and tables; a slider window would shrink them to a few cells. |
| 2026-10-20 | The dashboard opens on the last 2 hours | With hourly polls, a shorter range can briefly hold no poll and leave the latest-poll panels blank. |
| 2026-10-20 | LRF names on dashboards come from the LRF index itself, created in lookup mode and joined at query time (ES\|QL lookup join) | Kibana charts can't join two indices themselves, and ES\|QL can only join an index in lookup mode. Making the LRF index a lookup index keeps one copy of the LRF list, without a second index or a new field on every event. The cost is that the network heat map can't pick an LRF by clicking, so the LRF control is used instead. |
| 2026-10-20 | The Ducting Watch map takes each path's latest poll first and then keeps only paths at ducting or trapping | Filtering first would keep showing a path's last ducting poll after it had returned to normal. |
| 2026-10-20 | The co-channel verification map shows only the latest discovery run in the time range | Runs are appended daily; drawing every run would stack identical paths and hide gaps in the latest one. |
| 2026-10-21 | Lock loss on one channel counts as an anomaly, alongside BER spike and MER drop | Strong co-channel interference drives a channel to "No Signal"; ignoring unlocked reports would miss the worst cases. Frame and site loss are kept apart by the lock-loss guard. |
| 2026-10-21 | The environmental evidence is dM/dh < 0 (ducting or trapping) on any co-channel path on the channel, with trapping reported as the stronger evidence and the strongest path's transmitter named | Keeps the 2026-10-19 decision on dM/dh < 0 while using the published ducting state, so the verdict and the Ducting Watch map agree. |
| 2026-10-21 | Each LRF and channel has one of six verdicts, from `clear` to `cci_confirmed`, instead of a yes/no | Operators need to know why interference was *not* confirmed (one demod only, the other side silent, no ducting, ducting unknown) as much as when it was. |
| 2026-10-21 | One-sided verdicts wait 2 minutes (`pair_grace`) before being reported | The two sides rarely cross the 3-report mark in the same minute; without the wait, every A+B episode would start and end with a spurious "single side" minute. |
| 2026-10-21 | Lock loss counts only while another input on the same frame is locked (the lock-loss guard); full outage and frame-fault rules are deferred | The guard keeps a dark frame or site from ever confirming interference, while the outage rules are still open (Q22, Q25). |
| 2026-10-21 | Paths with no usable weather in a poll are kept as "ducting unknown" rather than left out | Leaving them out made their channel look as if it had no co-channel paths, which would report the degradation as unexplained instead of unknown. |
| 2026-10-21 | Phase 4b publishes verdict events (start, update, ongoing every 5 minutes, clear); alerting and inSITE integration stay in Phase 4c | Lets the rules be checked in Kibana now. Re-publishing ongoing episodes means a dashboard on a short time range still sees an active episode. |
| 2026-10-21 | The correlation engine's reasoning is published as a decision trace, in categories that can be turned on separately, with pair decisions and hysteresis on by default | Hits, misses and their reasons can be searched and shown on a dashboard instead of read from logs. The per-report category is off by default because of its volume. |
| 2026-10-21 | A correlation specification for operators, without program internals, ships with tropobeat ([CORRELATION.md](CORRELATION.md)) | The rules need to be understood and agreed by operations and RF engineering, not only by developers. |
| 2026-10-21 | The replay tool reproduces every verdict on purpose, with per-LRF mock weather, and can run endlessly for demos | Real ducting events can't be produced on demand; deterministic scenarios make every rule testable and demonstrable. |
| 2026-10-26 | Transmitters on the same channel within 1 km of an LRF's own station are treated as channel-sharing partners and excluded, like the own station | Stations that share one transmitter carry the LRF's own signal, not interference, but the demod tag names only one of them. Without this, 12 false co-channel paths appeared across the network (19 with Class A), for example WSBE-TV and WRIW-CD at Providence. |
| 2026-10-26 | Distributed-transmission sites (`DTS`) are part of the default FCC import | They are full-power transmitters: 31 full-power stations exist only as DTS sites, and leaving them out hid 76 co-channel paths across the network. The extra Open-Meteo usage is small, about 4%. Supersedes the `DTS` part of the 2026-10-08 filter decision. Resolves that part of Q11. |
| 2026-10-26 | Each site of a multi-site station gets its own path ID, with `:s<site>` appended (sites numbered strongest first, the same at every LRF), and every path carries a readable transmitter label | A station's sites share its facility ID and channel, so their paths would otherwise share an ID and get each other's weather and ducting results. Single-site IDs keep their form, so existing data and dashboards stay valid. The label ("WVER site 2 of 4 (5 kW)") makes the sites readable on dashboards and in correlator messages. |
| 2026-10-26 | When an index already exists, tropobeat adds new fields to its mapping instead of requiring a rebuild | A newer tropobeat can add fields (such as the FCC antenna ID) to an index created by an older one without losing its data. |

## 6. Known issues

- Source spreadsheet quirks are handled by tropobeat (Excel text encoding, 11 ZIP codes missing leading zeros, a leading space in one coordinate). Fixing them at the source would be cleaner.
- Elasticsearch authentication options are untested against a secured cluster (Q5).
- Development uses the free Open-Meteo API, which is for non-commercial use only. Production needs a commercial key (Q8, Q15).
- The default FCC filter excludes replacement translators (`DRT`), some of which run 10–15 kW on a channel other than their main station's; add them with `--services DTV,TV,DT,DTS,DRT` (Q11). Distributed-transmission sites (`DTS`) are included since 2026-10-26.
- The FCC station listing has no published format. If it changes, the import stops with an error rather than loading bad data. The FCC's bulk database download is the fallback source.
- Reference lookups support up to 10,000 records per set. Fine for the 211 LRFs; very large reference sets may need paging.
- Run the FCC import (cron) before the daily discovery time (e.g. FCC at 02:00, discovery at 03:00 UTC), so discovery always reads a complete, freshly refreshed transmitter list.
- Avoid `tropobeat insert … --recreate` while tropobeat is running: it briefly deletes the index. Normal re-runs are safe.
- The co-channel path line relies on a one-line patch to the workspace's copy of libbeat (allowing geographic-shape fields). Keep the patch when updating libbeat, or the build of the index template will fail.
- Ducting results are only as fine as the weather model's pressure levels (200–1,000 m apart near the ground). A thin, strong duct inside one layer is averaged with the air around it: a 50 m duct inside an otherwise normal 200 m layer reads as super-refractive, not ducting. Scores should be read as a likelihood (Q17).
- Because every detected duct spans at least one model layer (about 200 m or more), almost every duct counts as trapping VHF-high and UHF channels; only low-VHF channels (RF 2–6) are often reported as not trapped. The trapping flag will become more selective with finer vertical detail (Q17).
- The ducting score bands are initial values that haven't been calibrated against real ducting events yet (Q16).
- Demod telemetry facts from the 2026-10-13 sample: each input reports about once a minute, and about 21% of reports are `Not Locked` (ignored).
- The demod field holding Pre-FEC BER is `d_pre_fec_ber` (the pasted rules call it `f_pre_fec_ber`).
- A saturated BER reading of 16.777215 appears in the demod telemetry; it is treated as saturated, not as a value.
- Messages published before tropobeat's queue exists (first start-up) are not received; this is expected with an own queue.
- After upgrading to a version with new fields (such as the `baseline` metricset), load the new index template and roll the tropobeat data stream over, or the new fields get types guessed from their first values (for example a BER median of 0 mapped as a whole number). Seen on the test cluster.
- If tropobeat's queue settings are changed after it was created, the broker refuses the new declaration; delete the queue so it can be recreated.
- Picking a path on the dashboard map takes two clicks (the path, then the filter button in its tooltip). Filtering on a single click needs a paid Elastic licence.
- Some dashboard panels query the tropobeat indices by name. If tropobeat's index name changes, those panels need the same change.
- Kibana keeps unsaved dashboard edits in the browser. After the dashboard is reloaded from tropobeat, use Reset if Kibana reports unsaved changes.
- The correlation thresholds and timings are initial values; real multi-day telemetry is needed to tune them (Q26, Q28).
- When test telemetry is replayed with healthy readings for the scenario LRFs, each input's MER is set to its best value in the sample: the sample covers only minutes of one day, and its median can sit at the threshold of the multi-day baseline.
- After upgrading to this version, load the new index template and roll the tropobeat data stream over, so the `correlation` and `correlation_trace` fields (including the LRF location as a geographic point) get their proper types. Done on the test cluster on 2026-10-21.
- The Ducting Watch network heat map needs the LRF index in lookup mode. An LRF index created by an earlier version must be recreated once with the LRF load command's rebuild option, with tropobeat stopped.

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
| 2026-10-14 | Weather events now carry the path line and are timestamped at fetch time, with the forecast hour kept separately. Code comments added to the discovery and scheduling code. |
| 2026-10-14 | Phase 3 built: the ducting calculation (above-ground profile, refractivity, layer classes, duct types and strength, likelihood score, duct trapping per channel, path ducting index), run on every weather poll. Results published as a summary on `weather` events and in full as the new `ducting` metricset, with `ducting.publish` to choose all or notable locations. Added [DUCTING.md](DUCTING.md), the technical reference for scoring, classes and fields. Q16–Q19 added; Q10 extended. Calibration against real soundings is next. |
| 2026-10-15 | Added mock weather (`openmeteo.mock`) for building dashboards: simulated weather that produces every ducting state, including trapping, with a mixed mode that cycles each site through all states. Simulated events are tagged `mock-weather`. README updated. |
| 2026-10-19 | Phase 4 planned and split into 4a, 4b and 4c, using the supplied RF telemetry rules (baseline, dual-path confirmation, 3-minute hysteresis, outage exclusion). A week of test demod data (one real day copied) reviewed. Phase 4a built: the live demod stream from RabbitMQ with reconnects, daily 7-day baselines per demod input published as the new `baseline` metricset, baselines and ducting held in memory, and a replay tool. Verified against a local broker and the test cluster. Q20–Q26 added. |
| 2026-10-20 | Shared identifying fields on every result (LRF, path, channel, band, transmitters, own stations, discovery run, forecast hour), replacing the per-result ones, and a published four-step ducting state. Baselines now list the co-channel paths that could interfere with them, and the first baseline run waits for discovery. Dashboard design (LRF-focused heat maps and map) added to Phase 7. Test cluster's tropobeat data and index template rebuilt and verified. README and DUCTING.md updated. |
| 2026-10-20 | Ducting Watch dashboard built in Kibana from the dashboard design and shipped with tropobeat, with its own guide (DASHBOARD.md). The standard dashboard setup command now loads it; it had been switched off. Phase 7's dashboard item complete; the map item narrowed to what remains. |
| 2026-10-20 | Second dashboard, **Co-channel paths**, to check co-channel discovery. Ducting Watch map shows only paths that are ducting or trapping now; its network heat map labels each LRF with its site's city and state. The LRF index is now created in lookup mode so dashboards can join it for these labels. Both dashboards installed into a clean Kibana space with tropobeat's dashboard setup. |
| 2026-10-21 | Phase 4b built: the correlation engine judges each live demod report against its baseline (BER spike, MER drop, lock loss), applies the 3-report hysteresis, pairs sides A and B, and confirms co-channel interference when a co-channel path on the channel is ducting, naming the likely interferer. Six verdicts per LRF and channel, published as the `correlation` metricset (with ongoing re-publishing and the LRF location), and a decision trace (`correlation_trace`) in categories that can be turned on separately. New **Correlation debug** dashboard and [CORRELATION.md](CORRELATION.md) specification. Replay scenarios and a looping demo mode, with per-LRF and failing mock weather. Paths without usable weather now count as ducting unknown. Verified end to end against a local broker and the test cluster: every verdict, both power signatures, the ongoing and clear events, flapping, gaps, the missing side label and a mid-run weather outage; the remaining rare reasons (no weather poll yet, a channel received on one side only, baselines not usable) are covered by automated tests. The runs also tightened how a side that is recovering is treated, so a frame coming back after an outage raises nothing new. Outage and frame-loss rules deferred; Q28 and Q29 added. |
| 2026-10-26 | Co-channel discovery now excludes channel-sharing partners of the own station (same channel, within 1 km), removing 12 false paths. Measured the effect of adding Class A stations on paths and Open-Meteo usage (Q11, Q15). |
| 2026-10-26 | Distributed-transmission (`DTS`) sites added to the default FCC import (76 more co-channel paths). Multi-site stations get one path per site, with site-numbered path IDs and readable transmitter labels on every result and in correlator messages. Existing indices receive new fields automatically. Dashboards show the site labels (Phase 7). |
| 2026-10-27 | The replay tool can run several scenarios side by side, each on its own cycle, and a step can last for the whole replay. New scenario: a trapping CCI episode that never clears (PIT), to run next to the all-conditions or demo cycle. |
