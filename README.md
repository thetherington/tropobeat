# tropobeat

**Automated Tropospheric Ducting & Telemetry Correlation Engine** for Evertz Magnum Analytics (inSITE).

Tropobeat is an [Elastic Beat](https://www.elastic.co/beats) that watches for **tropospheric ducting**, the weather condition that lets distant TV transmitters reach a Local Receive Facility (LRF) on the same RF channel as the station it is meant to receive. It finds each LRF's co-channel paths, fetches the weather along them, works out how likely a duct is, and publishes the results to Elasticsearch for dashboards, maps and (in later phases) alerting.

This README describes **what tropobeat does today**. It is updated as phases in the [rolling plan](ROLLING_PLAN.md) are completed. For how the ducting calculation works, see **[DUCTING.md](DUCTING.md)**.

## Contents

1. [Why](#1-why)
2. [What tropobeat does today](#2-what-tropobeat-does-today)
3. [How it works](#3-how-it-works)
4. [Requirements](#4-requirements)
5. [Building](#5-building)
6. [Loading reference data: `tropobeat insert`](#6-loading-reference-data-tropobeat-insert)
7. [Running tropobeat](#7-running-tropobeat)
8. [Configuration](#8-configuration)
9. [What tropobeat publishes](#9-what-tropobeat-publishes)
10. [Logs](#10-logs)
11. [Development](#11-development)
12. [Project documents](#12-project-documents)

---

## 1. Why

VHF/UHF TV signals (54–608 MHz) normally stop at the radio horizon. Under certain weather conditions the air forms a **duct** that carries them hundreds of miles further. When a distant station arrives at an LRF on the same RF channel as the local station, the two collide (co-channel interference): MER collapses and the local channel pixelates or drops to "No Signal".

Tropobeat's goal is to move operations from reactive troubleshooting to proactive, data-driven root-cause analysis: detect ducting, explain which transmitter is interfering, and predict which LRFs are at risk.

## 2. What tropobeat does today

| Capability | Status |
|---|---|
| Load the LRF site list into Elasticsearch (`tropobeat insert lrfcoordinates`) | ✅ |
| Load FCC full-power TV transmitter locations into Elasticsearch (`tropobeat insert fcctransmitters`) | ✅ |
| Work out which RF channels (and stations) each LRF receives, from inSITE demod telemetry | ✅ |
| Daily co-channel discovery: same-channel transmitters within 200 miles of each LRF, excluding its own station, with geodesic path midpoints | ✅ |
| Hourly weather for every path point from the Open-Meteo API | ✅ |
| Ducting calculation on every weather poll: refractivity, layer classes, ducts, a ducting score per point and a ducting index per path | ✅ (calibration pending) |
| Correlating ducting with MER, Pre-FEC BER and RF power drops | 📋 planned (Phases 4–5) |
| Forecasting at-risk LRFs, alerting, inSITE dashboards | 📋 planned (Phases 6–7) |

See the [rolling plan](ROLLING_PLAN.md) for detailed status, open questions and decisions.

## 3. How it works

```
 REFERENCE DATA (loaded with `tropobeat insert`, e.g. from cron)
 ┌─────────────────────┐   ┌─────────────────────┐   ┌─────────────────────┐
 │      LRF list       │   │  FCC transmitters   │   │   Demod telemetry   │
 │  (lrf-coordinates)  │   │ (fcc-transmitters)  │   │  (already in inSITE)│
 └──────────┬──────────┘   └──────────┬──────────┘   └──────────┬──────────┘
            └─────────────────────────┼─────────────────────────┘
                                      ▼
                    ┌───────────────────────────────────┐
                    │  cochannel job                    │
                    │  daily at 03:00 UTC + at start-up │
                    └─────────────────┬─────────────────┘
                                      ▼
                    ┌───────────────────────────────────┐
                    │  Co-channel paths                 │──► `cochannel` events
                    │  LRF · midpoint · transmitter     │
                    └─────────────────┬─────────────────┘
                                      ▼
                    ┌───────────────────────────────────┐
     Open-Meteo ───►│  weather job, hourly              │──► `weather` events
                    │  + ducting calculation            │──► `ducting` events
                    └───────────────────────────────────┘
```

**Reference data.** Two `insert` commands load the reference data tropobeat works from: the LRF sites and the FCC transmitters. They run outside the beat (by hand or from cron) and can run while tropobeat is running. The demod telemetry is already collected by inSITE; tropobeat only reads it.

**The `cochannel` job** runs once at start-up and then daily (`cochannel.schedule`, default 03:00 UTC). Each run:

1. Re-reads the LRF list, so LRFs added or moved with `insert lrfcoordinates` are picked up without a restart.
2. Reads the last 24 hours of demod telemetry and lists, for each LRF, the RF channels its locked demod inputs receive and the call signs in each input's tag (for example `WTHI / WTHI2 (OTA)` → WTHI, WTHI2).
3. For each LRF and channel, finds every FCC transmitter on that channel within 200 miles (`cochannel.radius`).
4. Excludes the LRF's **own station**: the transmitter whose call sign matches the input tag (ignoring suffixes such as `-TV`). If the tagged station isn't a full-power FCC station (LPTV, Class A, translators, Mexican stations), nothing is excluded. Inputs with no call sign (`Spare`, `NA`) are skipped. If the matched own station is more than 150 miles from the LRF, the channel is skipped with a warning, because that points to a wrong LRF code or coordinates.
5. Calculates the WGS84 geodesic midpoint, distance (miles) and bearing of every remaining **co-channel path**.
6. Keeps the paths in memory for the weather job and publishes one `cochannel` event per path.

**The `weather` job** runs every `period` (default 1 hour). Its first run starts as soon as the first discovery run finishes. Each poll:

1. Builds a group of points for every co-channel path (LRF, midpoint, transmitter), plus one point for each LRF with no paths.
2. Fetches the weather for all points from Open-Meteo in batches, fetching each unique location once: 8 pressure levels (1000 to 700 hPa) with temperature, humidity and height, plus the surface.
3. Runs the **ducting calculation** on each location: refractivity, layer gradients and classes, ducts, and a 0–1 ducting score. Each path gets a ducting index and a flag for whether a duct is thick enough to trap its channel. See [DUCTING.md](DUCTING.md).
4. Publishes one `weather` event per path (or LRF) and `ducting` events with the full analysis.

Neither job ever overlaps itself: a run that is due while the previous one is still going is skipped. Stopping tropobeat waits for running work to finish.

## 4. Requirements

| Requirement | Notes |
|---|---|
| Elasticsearch 8.x | Holds the reference data and receives tropobeat's events. Tested with 8.17. |
| inSITE demod telemetry | The daily `log-metric-p-rfdemod-YYYY.MM.DD` indices, readable from the cluster set in `tropobeat.elasticsearch`. |
| Open-Meteo API | The free API is for non-commercial use only and limited to 10,000 calls a day. Production use needs a commercial API key; see the rolling plan (Q8, Q15) for expected usage. Weather polling can be turned off (`openmeteo.enabled: false`). |
| Outbound HTTPS | To `transition.fcc.gov` (FCC import) and `api.open-meteo.com` or `customer-api.open-meteo.com`. |
| To build: Go 1.23 and the Beats workspace | See [Building](#5-building). |

## 5. Building

Tropobeat builds inside a Go workspace alongside a checkout of [elastic/beats](https://github.com/elastic/beats):

```
tropobeat_workspace/
├── go.work        # uses ./beats and ./tropobeat
├── beats/         # elastic/beats checkout (libbeat)
└── tropobeat/     # this repository
```

```bash
cd tropobeat
go build -o tropobeat .      # or: mage build
```

> **libbeat patch.** The co-channel path line is mapped as a `geo_shape` so Kibana Maps can draw it. The workspace's libbeat needs a one-line change to accept that field type: in `beats/libbeat/mapping/field.go`, `case "geo_shape":` is added next to `case "geo_point":`. Without it, regenerating the fields (`mage update`) fails. Keep the patch when updating the beats checkout.

After changing `_meta/fields.yml` or the config templates in `_meta/config/`, regenerate `fields.yml`, `include/fields.go`, the docs and the `tropobeat*.yml` files:

```bash
mage update
```

> `mage update` **overwrites** `tropobeat.yml`, `tropobeat.reference.yml` and `tropobeat.docker.yml` from the templates. Copy any local edits first, or make them in `_meta/config/`.

## 6. Loading reference data: `tropobeat insert`

Both commands write directly to Elasticsearch, never to tropobeat's output, and don't need tropobeat to be running. They are safe to re-run: documents are keyed so that re-running updates them instead of duplicating them.

### Elasticsearch connection (both commands)

| Flag | Environment variable | Default | Meaning |
|---|---|---|---|
| `--es-host` | `ES_HOSTS` (comma-separated) | `http://localhost:9200` | Elasticsearch URL. Repeat the flag for several hosts. |
| `--es-username` | `ES_USERNAME` | | Basic-auth username. |
| `--es-password` | `ES_PASSWORD` | | Basic-auth password. |
| `--es-api-key` | `ES_API_KEY` | | API key (base64 `id:api_key`), instead of username and password. |
| `--es-ca-cert` | | | PEM CA certificate to trust for HTTPS. |
| `--es-insecure` | | `false` | Skip TLS certificate verification (testing only). |

Flags take precedence over environment variables. Prefer the environment variables for passwords and API keys, especially in cron, so they don't appear in process lists or crontabs. They are never shown in `--help`.

### `tropobeat insert lrfcoordinates`: LRF sites

Reads the LRF spreadsheet export (CSV) and loads it into the `lrf-coordinates` index, with each site's location as a `geo_point`.

```bash
# Preview: parse the file and print the sites, without connecting
./tropobeat insert lrfcoordinates -f "LRF Addresses_with_GPS_Updated.csv" --dry-run

# Load into a local, unsecured cluster
./tropobeat insert lrfcoordinates -f "LRF Addresses_with_GPS_Updated.csv"

# Load into a secured cluster
ES_PASSWORD='…' ./tropobeat insert lrfcoordinates -f "LRF Addresses_with_GPS_Updated.csv" \
  --es-host https://es.example.com:9200 --es-username tropobeat --es-ca-cert /etc/tropobeat/ca.pem
```

| Flag | Default | Meaning |
|---|---|---|
| `-f`, `--file` | (required) | Path to the LRF CSV file. |
| `-i`, `--index` | `lrf-coordinates` | Index to load into. If changed, set the same name in `tropobeat.lrf_index`. |
| `--dry-run` | `false` | Parse and print the sites without connecting to Elasticsearch. |
| `--recreate` | `false` | Delete and rebuild the index first (for example after a mapping change). Avoid while tropobeat is running. |

**CSV format.** Columns are matched by header name (case and spacing don't matter), so their order is free. Required: `DMA Name`, `Code`, `Physical Street Address`, `Physical City`, `State`, `Zip Code`, `Physical City Lattitude and Longitude` (`"lat, lon"` in decimal degrees). Optional: `Physical City GPS coordinates` (degrees/minutes/seconds, stored as text). The loader copes with Excel exports: Latin-1 text encoding, ZIP codes that lost their leading zeros (`4401` → `04401`), and stray spaces. A row with missing or out-of-range coordinates stops the load with an error naming the line.

Each site is stored with its LRF code as the document ID. The LRF codes must match the `s_lrf_id` values in the demod telemetry; see the rolling plan (Q14) for known mismatches.

### `tropobeat insert fcctransmitters`: FCC transmitters

Downloads the FCC TV station listing (the FCC TV Query service), one request per state, and loads the transmitters into the `fcc-transmitters` index: call sign, facility ID, service, status, RF and virtual channel, ERP, antenna heights, licensee, and location as a `geo_point`.

```bash
# Preview two states without loading
./tropobeat insert fcctransmitters --states RI,CT --dry-run

# Load every state (about 52 requests, 1.5 s apart)
./tropobeat insert fcctransmitters

# Include distributed-transmission sites and replacement translators
./tropobeat insert fcctransmitters --services DTV,TV,DT,DTS,DRT
```

| Flag | Default | Meaning |
|---|---|---|
| `--states` | 50 states, DC and PR | States and territories to fetch. |
| `--services` | `DTV,TV,DT` | FCC service codes to keep. Others include `DTS` (distributed transmission sites), `DRT` (replacement translators), `DCA` (Class A), `LPD` (low power). |
| `--statuses` | `LIC` | Record statuses to keep, e.g. `LIC,CP` to add construction permits. |
| `-i`, `--index` | `fcc-transmitters` | Index to load into. If changed, set the same name in `tropobeat.fcc_index`. |
| `--dry-run` | `false` | Fetch and print the transmitters without loading. |
| `--recreate` | `false` | Delete and rebuild the index first. Avoid while tropobeat is running. |
| `--no-prune` | `false` | Keep transmitters the FCC no longer lists. |
| `--delay` | `1.5s` | Minimum time between FCC requests. |
| `--timeout` | `30s` | Timeout for each FCC request. |
| `--user-agent` | `tropobeat (FCC TV facility sync; …)` | User-Agent sent to the FCC. |

How it behaves:

- **Filtering.** By default only licensed full-power stations are kept, about 1,750 transmitters nationwide. Some full-power stations exist only as `DTS` sites and are left out unless `DTS` is added (see the rolling plan's known issues and Q11).
- **Updates and pruning.** Each transmitter's document ID is its facility ID plus location, so re-runs update it, and stations sharing a facility ID keep one document per site. After a **fully successful** run, transmitters that are no longer listed are deleted, but only within the states fetched in that run. A state that fails, or returns an empty listing, blocks pruning.
- **Being a good client.** Requests identify tropobeat honestly, keep TLS certificate checks on, wait at least `--delay` between requests, and back off on "too many requests" or server errors (honouring `Retry-After`). If the FCC refuses a request (HTTP 403), the run stops rather than retrying.
- **Exit code.** Non-zero if any state failed, so cron can report it.
- **Format changes.** The FCC listing has no published format. If its layout changes, the import stops with an error rather than loading bad data.

**Running from cron.** Run the import before the daily discovery time, so discovery always reads a complete, freshly pruned list:

```cron
# /etc/cron.d/tropobeat-fcc: FCC import daily at 02:00 UTC (discovery runs at 03:00)
ES_HOSTS=https://es.example.com:9200
ES_USERNAME=tropobeat
ES_PASSWORD=…
0 2 * * * root flock -n /tmp/tropobeat-fcc.lock /opt/tropobeat/tropobeat insert fcctransmitters >>/var/log/tropobeat/fcc.log 2>&1
```

## 7. Running tropobeat

```bash
./tropobeat -e -c tropobeat.yml
```

`-e` logs to the console. As a service, tropobeat runs like any other Beat (for example under systemd). The `insert` commands can run at any time alongside it: they don't use tropobeat's data directory or its lock.

**First run, step by step:**

1. Load the LRFs: `tropobeat insert lrfcoordinates -f …`
2. Load the FCC transmitters: `tropobeat insert fcctransmitters`
3. Set `tropobeat.elasticsearch` (where the reference data and demod telemetry are) and `output.elasticsearch` (where events go) in `tropobeat.yml`.
4. For a first trial, limit tropobeat to a few LRFs: `lrf_sites: ["TERH", "PROV"]`.
5. Start tropobeat.

At start-up tropobeat checks the LRF list and stops with a clear message if the cluster is unreachable or the index is missing or empty.

**Useful one-off overrides** (`-E` overrides any setting):

```bash
# Discovery only, no Open-Meteo calls
./tropobeat -e -E tropobeat.openmeteo.enabled=false

# Trial on three LRFs
./tropobeat -e -E 'tropobeat.lrf_sites=["TERH","PROV","BOS"]'
```

## 8. Configuration

Settings live under `tropobeat:` in `tropobeat.yml`. `tropobeat.reference.yml` lists every setting with comments. Elasticsearch output, logging and other general settings are the standard Beats ones.

### General

| Setting | Default | Meaning |
|---|---|---|
| `period` | `1h` | Weather poll interval. Each poll costs Open-Meteo calls per location, so check your plan's limits before shortening it. |
| `lrf_sites` | (all) | Limit discovery and weather to these LRF codes, for trials and testing. Codes are case-insensitive; unknown codes are logged; if none are valid, tropobeat stops. |
| `lrf_index` | `lrf-coordinates` | Index holding the LRF sites. |
| `demod_index` | `log-metric-p-rfdemod-*` | Index pattern holding the demod telemetry. |
| `fcc_index` | `fcc-transmitters` | Index holding the FCC transmitters. |

### `elasticsearch`: where reference data is read from

Separate from tropobeat's output, so the reference data can live on a different cluster or need different credentials.

| Setting | Default | Meaning |
|---|---|---|
| `hosts` | `["http://localhost:9200"]` | Elasticsearch URLs. |
| `username`, `password` | | Basic authentication. Keep the password in the keystore: `tropobeat keystore add ES_PASSWORD`, then `password: "${ES_PASSWORD}"`. |
| `api_key` | | API key, instead of username and password. |
| `ca_cert` | | PEM CA certificate to trust. |
| `insecure` | `false` | Skip certificate verification (not for production). |

### `cochannel`: daily discovery

| Setting | Default | Meaning |
|---|---|---|
| `schedule` | `"0 3 * * *"` | Cron expression (UTC). Discovery also runs once at start-up. |
| `radius` | `"200mi"` | Search radius around each LRF. |
| `demod_window` | `24h` | How far back the demod telemetry is read. |

### `openmeteo`: weather

| Setting | Default | Meaning |
|---|---|---|
| `enabled` | `true` | `false` turns weather polling off: co-channel discovery only, no Open-Meteo calls, no API key needed. |
| `api_key` | | Commercial API key. When set, requests go to `customer-api.open-meteo.com`. Keep it in the keystore. The key is never written to logs or errors. |
| `url` | (Open-Meteo) | Override the endpoint, e.g. a self-hosted Open-Meteo. |
| `models` | (best match) | Weather models to query. |
| `pressure_levels` | `[1000, 975, 950, 925, 900, 850, 800, 700]` | Pressure levels (hPa) requested, in addition to the surface. |
| `batch_size` | `50` | Locations per request. |
| `forecast_days` | `1` | Days of hourly data requested. |
| `timeout` | `10s` | Timeout for each request. |
| `batch_delay` | `100ms` | Pause between requests. |
| `max_retries` | `2` | Retries after a rate limit, server or network error (including timeouts). |
| `coordinate_precision` | `0` (off) | Round coordinates before de-duplicating, so nearby path points are fetched once. `0.01`° (about 1 km) saved about 8% of locations across the network, `0.05`° about 17%. Values above `0.1` may merge points in different weather-model grid cells. |

### `ducting`: ducting calculation

See [DUCTING.md](DUCTING.md) for what these mean.

| Setting | Default | Meaning |
|---|---|---|
| `publish` | `all` | Which locations get a `ducting` event: `all`, or `notable` (super-refractive and ducting only). Weather events carry the ducting summary either way. |
| `min_layer_m` | `20` | Levels closer than this to the point below are dropped, to avoid noisy gradients. |
| `score.super_refractive_n_per_km` | `-79` | Gradient (N-units/km) where the score starts rising from 0. |
| `score.ducting_n_per_km` | `-157` | Gradient at which the score reaches `ducting_score`. |
| `score.strong_n_per_km` | `-300` | Gradient at which the score reaches 1. |
| `score.ducting_score` | `0.6` | Score at the ducting threshold. |

The score bands are initial values, to be calibrated against weather-balloon soundings (rolling plan, Q16).

## 9. What tropobeat publishes

Events go to tropobeat's output, by default the `tropobeat-<version>` data stream (for example `tropobeat-8.18.0`). Each event has `metricset.name` set to one of:

| Metricset | One event per | Main fields |
|---|---|---|
| `cochannel` | Co-channel path, per discovery run | `tropobeat.cochannel.run_id`, `path_id` (`<lrf>:<facility id>:<rf channel>`), `rf_channel`, `lrf.{code, location, call_signs}`, `transmitter.{call_sign, facility_id, service, erp_kw, location}`, `midpoint.location`, `distance_mi`, `bearing_deg`, `path` (the path as a line) |
| `weather` | Co-channel path (or LRF with no paths), per poll | `tropobeat.weather.path_id`, `run_id`, `lrf_code`, `rf_channel`, `transmitter_call_sign`, `forecast_time`, `path`; `lrf`/`midpoint`/`transmitter`, each with `location`, `elevation_m`, `surface` and `levels[]` (pressure, height, temperature, humidity) and a `ducting` summary; and the path's `ducting.{index, frequency_mhz, trapped}` |
| `ducting` | Unique weather location, per poll | The full analysis: refractivity at every height, every layer's gradient and class, the ducts found, the score and class, and the paths and LRFs that use the location. See [DUCTING.md](DUCTING.md#7-field-reference). |

Use the latest `run_id` for the current set of co-channel paths: each discovery run publishes a fresh, dated set rather than updating the previous one.

**Maps.** Every `location` is mapped as a `geo_point`, and the `path` fields as a `geo_shape`, so Kibana Maps can show LRFs, midpoints and transmitters as points and draw every co-channel path as a line. [DUCTING.md](DUCTING.md#9-kibana-maps-setup) describes a ducting map.

## 10. Logs

Each run logs a summary, which is the quickest way to see what tropobeat is doing:

```
Co-channel discovery 20261013T184527Z: 211 LRFs, 201 with demod channels, 1091 channels searched,
  1001 own-station transmitters excluded, 1829 co-channel paths published (3.084s)
Weather poll: 1854 groups (1829 co-channel paths, 25 LRF-only); points: 5512 requested,
  2589 unique after merge, 52 requests; …
```

Warnings worth acting on:

| Message | Meaning |
|---|---|
| `demod LRF IDs not in lrf-coordinates (skipped): …` | The demods report LRF codes that aren't in the LRF list. Those sites are skipped. See rolling plan Q14. |
| `skipped <LRF> RF <n>: own station … is N mi away …` | The LRF's own station is implausibly far away, which points to a wrong LRF code or coordinates. That channel is skipped. |
| `lrf_sites: codes not found …` | A code in `lrf_sites` isn't in the LRF list. |
| `Job <name> failed: …` | A run failed. The next run is still scheduled, and the previous co-channel paths stay in use. |

Inputs with no call sign (`Spare`, `NA`) and channels whose station isn't a full-power FCC station are logged at info and debug level.

## 11. Development

```bash
go vet ./...
go test ./...

# Live tests against the real Open-Meteo API (a handful of requests)
OPENMETEO_LIVE=1 go test -run TestLive ./internal/openmeteo/
```

| Path | Contents |
|---|---|
| `beater/` | The beat: scheduler, the `cochannel` and `weather` jobs, event building |
| `cmd/insert/` | The `tropobeat insert` commands |
| `config/` | Configuration and defaults |
| `internal/lrf/` | LRF model, CSV loader |
| `internal/fcc/` | FCC TV Query client and parser |
| `internal/cochannel/` | Demod channel query, co-channel discovery, geodesic midpoints |
| `internal/openmeteo/` | Open-Meteo client (fetches and organises weather only) |
| `internal/ducting/` | Refractivity, layers, ducts and scores |
| `internal/elastic/` | Shared Elasticsearch client and helpers |
| `_meta/` | Field definitions and config templates (source for `mage update`) |
| `scripts/` | The original Python prototypes |

## 12. Project documents

| Document | What's in it |
|---|---|
| [ROLLING_PLAN.md](ROLLING_PLAN.md) | Phases and status, open questions, decisions, known issues, change log |
| [DUCTING.md](DUCTING.md) | How the ducting calculation works, classes and scores, field reference, reading the results, Kibana Maps setup |
| `tropobeat.reference.yml` | Every setting, with comments |
