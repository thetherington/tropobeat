# Plan: Phase 2 co-channel discovery + Phase 1 prerequisites (scheduler, demod query, weather poll)

> **Status:** implemented in commit `cf030f3` (2026-10-13). This is the working plan as approved on 2026-10-13, kept for reference. For project status and open questions, see [ROLLING_PLAN.md](../../ROLLING_PLAN.md); for the ducting work that followed, see [phase-3-ducting.md](phase-3-ducting.md).
>
> **Where the build differs from this plan:**
> - **Call signs.** `ParseCallSigns` keeps every call sign in a tag (`WTHI / WTHI2 (OTA)` → WTHI, WTHI2), not just the first, and also accepts Mexican `X` call signs (`^[KWX][A-Z0-9]{2,5}$`).
> - **Own-station check.** The own station is looked up nationwide on its channel, not only among the in-radius hits, so swapped LRF codes (MON/MONR, about 1,700 mi away) are caught by the 150-mile check. There is no nearest-hit fallback, so its planned test was dropped.
> - **First weather poll.** The weather job doesn't start immediately; its first poll runs when the first discovery run finishes, so it covers the co-channel paths from the outset.
> - **Event fields.** Path distance is in miles (`distance_mi`), not km. Since 2026-10-14 (`e55b497`), weather events are timestamped at fetch time with `forecast_time` kept separately, and carry the path line too.
> - **Added after the plan.** `openmeteo.enabled` turns weather polling off and runs co-channel discovery only.
> - **Measured figures.** On the full network: 1,829 co-channel paths, 5,512 path points, 2,589 unique weather locations, about 7,000 Open-Meteo calls per poll (about 5.0 million a month hourly). These replace the plan's estimates (about 1,879 paths, 3,182 points, 8,600 calls, 6.2 million a month).

## Context
Phase 2 of `ROLLING_PLAN.md` designs a daily job that, for every LRF, finds same-channel FCC transmitters within 200 miles, excludes the LRF's own station, computes each path's geodesic midpoint, and keeps the resulting LRF/midpoint/transmitter groups in memory for the weather poller. The groups are also published as a `cochannel` metricset.

Already built:
- `fcc-transmitters` index (`insert fcctransmitters`)
- `openmeteo.Client.GroupProfiles`
- LRF loading at beat start-up

The discovery job depends on three Phase 1 items that aren't built: the demod received-channel query, the gocron scheduler and the weather poll job. You chose to build them together, with the demod query and discovery as **one combined daily job**. The outcome is a beat that, end to end:
1. on start-up and daily, re-fetches the LRF locations, queries the demods, discovers co-channel paths, holds them in memory and publishes them
2. on each poll, fetches weather for every path point in batches and publishes it

## Findings from the real demod export (`sample/log-metric-p-rfdemod-2026.10.13.tar.gz`, gitignored)
41,667 docs from about 15 minutes (14:57–15:11 UTC, 2026-10-13); 2,812 demod inputs, each reporting about **once a minute**.
- **Mapping:** every `s_*` field is `keyword` and every `i_*` field is `integer` (index dynamic templates). Use `rfdemod.rf_demod.s_lrf_id`, `i_rf_channel`, `s_input_tag` and `s_input_status` directly. **No `_field_caps` helper is needed.**
- **Status:** 79% `Locked`, 21% `Not Locked` (excluded). 14 docs have no `s_lrf_id` (skipped); 3,234 have no tag (mostly unlocked).
- **LRF ID coverage:** 207 distinct IDs, 202 matching the CSV.
  - 5 are the same sites under different codes: `BIRM`→`BIR`, `HTSBG`→`HSTBG`, `LUB`→`LUBB`, `PRESQ`→`PRSQ`, `RNO`→`RNC`.
  - **`MON` and `MONR` are swapped:** in the CSV, `MON` = Monroe LA and `MONR` = Monterey CA; the demods use them the other way round.
  - 4 CSV LRFs had no demod data in the window: `GLEN`, `GRNNC`, `VICT`, `WATER`.
- **Channels:** 1,117 locked (LRF, channel) pairs across 201 LRFs, 1–17 channels per LRF (median about 5). 109 pairs carry more than one tag, mostly whitespace variants (`KOLR (OTA)` / `KOLR ( OTA )`).
- **Tags vs FCC (Q13)**, after fetching all 52 jurisdictions (1,749 full-power transmitters):
  - **990 / 1,117 (89%)** match a full-power FCC station on the same RF channel.
  - 121 parse to call signs that aren't in the full-power set: LPTV/Class A/DTS/translators (e.g. `K22JA`, `KQSL`) or foreign (`XHAS`, Tijuana).
  - 6 have no call sign: `Spare`, `NA`, `QUINTEKA/B`, empty.
  - Tags such as `WMTW(OTA)` (no space before `(`) need `(` and `)` treated as separators.
- **Scale:** after own-station exclusion, about **1,879 co-channel paths** within 200 mi involving **1,092 unique transmitters**, so about **3,182 weather points** per poll (211 LRFs + 1,092 transmitters + 1,879 midpoints).
- **API budget:** Open-Meteo counts calls **per location**, and more than 10 variables counts as multiple calls. At 27 variables that's about 2.7 × 3,182 ≈ **8,600 calls per poll**: hourly ≈ 6.2M/month (above the Professional plan's 5M), every 3 h ≈ 2.1M/month, every 6 h ≈ 1.0M/month. Hourly forecast series mean one poll can cover several hours, so a longer interval loses little (Q8, Q9).

## Part 0: move custom packages under `internal/` (first, as its own step)
Go's `internal/` rule means only code inside this module can import these packages, which is all that uses them today (`beater/`, `cmd/`, `config/`).

| From | To | New import path |
|---|---|---|
| `lrf/` | `internal/lrf/` | `github.com/thetherington/tropobeat/internal/lrf` |
| `fcc/` (incl. `testdata/`) | `internal/fcc/` | `…/internal/fcc` |
| `elastic/` | `internal/elastic/` | `…/internal/elastic` |
| `beater/openmeteo/` (incl. `testdata/`) | `internal/openmeteo/` | `…/internal/openmeteo` |

- Use `git mv`, so file history follows the move.
- Update the imports in the 7 files that use them: `beater/tropobeat.go`, `config/config.go`, `cmd/insert/lrfcoordinates/lrfcoordinates.go`, `cmd/insert/fcctransmitters/fcctransmitters.go`, `internal/lrf/elastic.go`, `internal/lrf/elastic_test.go` and `internal/openmeteo/example_test.go` (an external `openmeteo_test` package, which can still import `internal/` from inside the module).
- Package names stay the same (`lrf`, `fcc`, `elastic`, `openmeteo`), so only import lines change. Run `gofmt` afterwards to re-sort the import blocks.
- `config/` stays at the top level, following the libbeat beat-layout convention.
- Check with `go build ./... && go vet ./... && go test ./...` before starting Part A, so the move is verified on its own.
- `ROLLING_PLAN.md`: package references move to their `internal/` paths. This is done as part of the Part E rewrite.

## Part E: make `ROLLING_PLAN.md` customer-friendly (do this right after Part 0)
The rolling plan becomes a customer-facing document. It describes **what the system does, why, its status, and decisions**. **Package and library names may appear, briefly**, to say what is used, but not how it is called. Removed detail is **dropped**, not moved, because it already lives in this working plan, the code and its comments.

**Allowed, briefly:**
- Library names, e.g. "scheduled with gocron", "midpoints computed with tidwall/geodesic (WGS84)", "official Elasticsearch Go client". One short mention, no versions.
- Package names, e.g. "the `internal/openmeteo` package fetches weather; `internal/ducting` will compute ducting". Updated to the new `internal/` paths from Part 0.

**Remove everywhere:**
- **Version numbers and pins:** gocron v2.20.0, go-elasticsearch v8.17.0, geodesic versions, Go toolchain versions, "requires Go 1.21.4" and so on.
- **API calls, function/type/method names and signatures:** `CronJob(…)`, `WithSingletonMode(…)`, `WGS84.Inverse(…)/Direct(…)`, `GroupProfiles`, `lrf.Load`, `SearchAll`, `Complete()`, `_msearch`/composite-aggregation mechanics. Describe the behaviour instead.
- **Build and tooling detail:** `go.mod`, `go get`, `mage update`, `gofmt`, file names (`*.go`, `_meta/*.tmpl`), unit-test/fixture mechanics, and dependency-upgrade notes (e.g. the clockwork bump).

**Keep, in plain language:**
- **Behaviour and schedules.** For example, the scheduler row becomes: "All recurring work runs on a gocron scheduler inside tropobeat: discovery daily at 03:00 UTC and at start-up, weather polling every hour. A slow run is never overlapped by the next one, and shutdown waits for running work to finish."
- **Physics and RF domain content** (refractivity, the ITU-R P.453 thresholds, the CCI signature).
- **Names operators use:** the `tropobeat insert …` commands, `tropobeat.yml` setting names, Elasticsearch index names (`lrf-coordinates`, `fcc-transmitters`, `log-metric-p-rfdemod-*`), the `cochannel`/`weather` metricsets, and the demod field names.
- **Data facts and examples** (TERH/WTHI-TV, Denver below-ground levels, the 211 LRFs), status icons, open questions and known issues. Known issues are reworded by impact, e.g. "reference lookups support up to 10,000 records per set; fine for the 211 LRFs".

**Terminology (confirmed):** an LRF ↔ same-channel transmitter link within the search radius is a **co-channel path** everywhere, matching the `cochannel` metricset, `path_id` and the `path` line field. Replace the three current "threat" uses:
- Phase 2 "co-channel threat set" → "the LRF's co-channel paths"
- Phase 5 "on a co-channel threat path" → "on a co-channel path"
- Phase 7 "active threat paths" → "co-channel paths, highlighting those with active ducting"

"Threat" is used only for an **active ducting event on a path** (Phase 5/6 wording), never for the path itself. Verification: `grep -n -i 'threat' ROLLING_PLAN.md` shows only event-related uses.

**Section by section:**
- **Phases 0–8:** reword every Notes cell; same rows and statuses.
- **Phase 3 title:** "Tropospheric ducting calculation (`internal/ducting`)".
- **Decisions:** keep each decision and its reason, briefly. Library choices stay, with the library named and no version (e.g. "Recurring work runs on gocron: cron schedules, run-at-start, no overlapping runs, clean shutdown"). Purely internal pins (the Elasticsearch client version) lose their version detail and are folded into a brief row.
- **Change log:** keep the dates. Strip versions and API detail, and merge purely technical entries (e.g. "gocron pinned to v2.20.0") into the relevant entry.
- **Part D status updates** after implementation must follow the same style.
- Add one brief Decisions row: "Tropobeat's own packages live under `internal/`".

**Memory:** save a feedback memory: "ROLLING_PLAN.md is customer-facing: library and package names are fine, briefly; no version numbers, API calls, function names, file names or build/tooling detail; describe behaviour, status and decisions in plain language". Link it from the existing project memory.

**Verification for Part E:**
- `grep -n -E 'v[0-9]+\.[0-9]+\.[0-9]+|Go 1\.[0-9]+|clockwork|go\.mod|go get|mage|gofmt|\.go\b|\.tmpl|GroupProfiles|SearchAll|lrf\.Load|CronJob|WithStart|Singleton|WGS84\.(Inverse|Direct)|_msearch|composite' ROLLING_PLAN.md` returns nothing.
- Package paths in the rolling plan use `internal/…` (no `beater/openmeteo`, `beater/ducting`, or top-level `lrf/`/`fcc/`/`elastic/`).
- Every phase row, open question, decision date and status icon from before is still present (compare row counts per section before and after).
- Tables keep consistent cell counts.

## New dependencies
- `github.com/go-co-op/gocron/v2@v2.20.0`. It bumps `jonboulle/clockwork` from v0.2.2 to v0.5.0; libbeat's rate-limit processor only uses `Clock` and `NewRealClock()`. Verify with `go test ./libbeat/processors/ratelimit/` in `beats/`.
- `github.com/tidwall/geodesic@v1.52.4` (latest; Go 1.17+, MIT; v1.52.1–3 are retracted). Verified `WGS84.Inverse(lat1, lon1, lat2, lon2, &s12, &azi1, &azi2)` and `WGS84.Direct(lat1, lon1, azi1, s12, &lat2, &lon2, &azi2)`.

## Part A: `internal/cochannel/` package (new, alongside `internal/lrf` and `internal/fcc`)
| File | Contents |
|---|---|
| `internal/cochannel/received.go` | `Received{LRFCode, RFChannel, Tags []string, CallSigns []string}`; `LoadReceived(ctx, es, index string, window time.Duration) ([]Received, error)`; `ParseCallSign(tag string) (string, bool)` |
| `internal/cochannel/geo.go` | `Midpoint(lat1, lon1, lat2, lon2) (midLat, midLon, distM, bearingDeg)` using `geodesic.WGS84` (Inverse, then Direct at half the distance) |
| `internal/cochannel/discover.go` | `Discover(ctx, es, fccIndex string, lrfs []lrf.LRF, received []Received, radius string) (Result, error)`; own-station exclusion |
| `internal/cochannel/path.go` | `Path` (LRF code and location, RF channel, transmitter fields, midpoint, distance, bearing); `Path.ID()` = `<lrf>:<facility>:<rf>`; `Path.Group() openmeteo.Group` with points labelled `lrf`, `midpoint`, `transmitter` |

**Demod query (`LoadReceived`)**
- Index pattern from config (`log-metric-p-rfdemod-*`).
- Filters: `@timestamp >= now-<window>` (default 24h) and `rfdemod.rf_demod.s_input_status = Locked`.
- A composite aggregation over `s_lrf_id`, `i_rf_channel` and `s_input_tag`, paged with `after_key`.
- **Fields are keywords and integers** (confirmed by the export), so the aggregation uses `rfdemod.rf_demod.s_lrf_id`, `i_rf_channel` and `s_input_tag` directly.
- **LRF ID mismatches are not resolved in code** (left as a rolling-plan question, Q14). Demod IDs not in `lrf-coordinates` (`BIRM`, `HTSBG`, `LUB`, `PRESQ`, `RNO`) are logged and skipped. The MON/MONR swap is caught by the distance sanity check below.
- Rows are grouped per (LRF, channel); multiple demods or tags on the same channel are merged, with whitespace normalised.

**Call-sign parsing:** split the tag on whitespace, `/`, `(` and `)`, uppercase it, and keep **every** token matching `^[KW][A-Z0-9]{2,5}$`, so multi-station tags like `KRBK / KOZL (OTA)` give both. `WTHI / WTHI2 (OTA)` → {`WTHI`, `WTHI2`}, `WMTW(OTA)` → {`WMTW`}, and `Spare`/`NA`/`QUINTEKA` → none. 1,211 of 1,227 real tags parse.

**Discovery (`Discover`):**
- For each (LRF, received channel), a `geo_distance` (`radius`, default `200mi`) + `term rf_channel` query on `fcc-transmitters`.
- Queries are batched through `_msearch`, 100 per request, via a new `elastic.MultiSearch[T](ctx, es, index, queries []string) ([][]T, error)` in `internal/elastic/search.go`.
- Hits are decoded as `fcc.Document`.
- **Own-station exclusion** (revised from the real data):
  - **Call sign parsed and matched** (89%): drop every hit whose call sign, with any `-XX` suffix removed, is in the parsed set.
  - **Call sign parsed but not in `fcc-transmitters`** (LPTV/Class A/DTS/translator/foreign, about 11%): exclude nothing. The LRF's own transmitter isn't in the index, so every hit is a genuine full-power co-channel transmitter. Counted in the run summary. The old "drop the nearest" fallback would wrongly remove a real interferer here.
  - **No call sign** (`Spare`, `NA`, `QUINTEKA/B`, empty): **skip that channel and log it** (confirmed). These are spare or test inputs, not monitored services.
  - **Sanity check:** if the matched own station is more than 150 mi from the LRF, **skip that channel** and log a warning ("possible LRF code or coordinate mismatch"). Otherwise the search would centre on the wrong site and invent paths. This catches the MON/MONR swap (matches about 1,700 mi away) until Q14 is resolved.
- The remaining hits become `Path`s via `Midpoint`.
- `Result{Paths, Warnings, UnknownLRFs}`. `UnknownLRFs` lists `s_lrf_id` values that aren't in `lrf-coordinates`; they are logged, not dropped silently.

## Part B: beat scheduling and jobs (`beater/`)
- **`beater/scheduler.go`:** `newScheduler(logger)` → `gocron.NewScheduler(WithLocation(time.UTC), WithLogger(logpAdapter), WithStopTimeout(30s))`. A small adapter maps gocron's `Debug/Info/Warn/Error(msg, args...)` onto `*logp.Logger` (`Debugw` and so on).
- **`beater/tropobeat.go`:**
  - `New` creates the scheduler.
  - `Run` connects the publisher and does an initial LRF load (filtered by `lrf_sites`) as a start-up check (unchanged: an unreachable cluster or empty index still stops the beat). The result seeds the in-memory snapshot, so the weather job has LRFs before the first discovery run finishes. `Run` then registers two jobs, calls `Start()`, and waits on `ctx.Done()`.
  - `Stop` cancels the context and calls `scheduler.Shutdown()`, then closes the client.
  - **The temporary "test the first LRF with Open-Meteo" block is removed**, replaced by the real weather job.
  - Each job is created with `WithName`, `WithSingletonMode(LimitModeReschedule)`, `WithContext(bt.ctx)`, `WithEventListeners(AfterJobRunsWithError(log))` and `WithStartAt(WithStartImmediately())`.
- **`cochannel` job** (`beater/jobs.go`), `CronJob(cfg.Cochannel.Schedule, false)`, run immediately at start-up:
  1. **Fetch the LRF locations** with `lrf.Load(ctx, es, cfg.LRFIndex)` on every run, then apply the `lrf_sites` filter (one shared helper, also used at start-up), so LRFs added, moved or removed with `insert lrfcoordinates` are picked up without restarting the beat. An error or an empty result aborts this run and keeps the previous snapshot.
  2. `cochannel.LoadReceived`, then `cochannel.Discover` with the freshly loaded LRFs.
  3. Build a `snapshot{RunID, At, LRFs, Paths}` and swap it into `bt.state atomic.Pointer[snapshot]`. LRFs and paths are swapped together, so the weather job never sees paths for an LRF list they weren't built from.
  4. `PublishAll` one event per path.
  5. Log totals: LRFs loaded, LRFs with channels, channels, paths, exclusions, warnings, unknown LRFs.
  - On error, the previous snapshot is kept.
- **`weather` job**, `DurationJob(cfg.Period)`, run immediately:
  - Groups = one `Path.Group()` per path in the current snapshot, plus an LRF-only group (`lrf` point) for every LRF in the snapshot's LRF list that has no path. Before the first discovery run, the snapshot holds only the start-up LRF load and no paths, so every LRF gets an LRF-only group, which is the fallback.
  - Calls `bt.weather.GroupProfiles(ctx, groups, now)` and publishes **one event per group**. A partial failure publishes complete groups and logs the error.
- **Event shapes**, built in `beater/events.go`:
  - `metricset.name: cochannel`, `tropobeat.cochannel.*`: `run_id`, `path_id`, `lrf.code`, `lrf.location`, `lrf.call_signs`, `rf_channel`, `transmitter.{call_sign, facility_id, service, erp_kw, location}`, `midpoint.location`, `distance_km`, `bearing_deg`, and **`path`**: a GeoJSON `LineString` (LRF → midpoint → transmitter, `[lon, lat]` order) mapped as `geo_shape`, so one Kibana Maps layer can draw every co-channel path as a line. Maps can't draw a line between two `geo_point` fields in one document.
  - `metricset.name: weather`, `tropobeat.weather.*`: `path_id`, `run_id`, `lrf.code`, `rf_channel`, `forecast_time`, plus `lrf`/`midpoint`/`transmitter` objects, each with `location`, `elevation_m`, `surface.{pressure_hpa, height_m, temperature_k, relative_humidity_pct}` and `levels[]` (same four fields).
  - The event timestamp is the run or forecast hour.

## Part F: optional grid merge in `internal/openmeteo` (reduces billed calls)
Exact deduplication already happens in `GroupProfiles`/`Hourly`, along with batching (50 per request). With real data, deduplication cuts about 5,640 path points to about 3,200. Batching reduces HTTP requests but not billed calls, because Open-Meteo bills per location. This adds **optional rounding before deduplication**, so points that fall in the same weather-model grid cell are fetched once.
- **`openmeteo.Config.CoordinatePrecision`** (`coordinate_precision`, degrees, default `0` = off, so behaviour is unchanged). Each coordinate is rounded to the nearest multiple of the precision (e.g. `0.01` ≈ 1.1 km, `0.05` ≈ 5.5 km) and the rounded value is used as the dedupe key and in the request. The model grid is coarser than these values (several km), so the weather returned is effectively the same.
- **Mapping back:** `GroupProfiles` rounds each point's coordinate with the same function to find its profile, and still returns each point's original coordinate. For `Profiles`/`Hourly` with rounding on, `Requested` holds the rounded coordinate (documented). With rounding off, nothing changes.
- **Validation:** precision must be ≥ 0 and ≤ 1°; values above about 0.1° (~11 km) log a warning that points may be merged across grid cells.
- **Logging:** the weather job logs `points: N requested, M unique after merge, K requests` each run, so the saving is visible.
- **Tests:**
  - two points 300 m apart with `0.01` give one location in the request and both groups get the profile
  - `0` keeps both
  - rounding is symmetric for negative longitudes
  - the original coordinates are preserved in `PointProfile`
- **Config templates:** `#coordinate_precision: 0.01` in the reference config, explained as "merge points closer than about this many degrees to cut Open-Meteo API usage; 0 = off".
- **Rolling plan:** a Phase 1 row ("optional grid merge to reduce Open-Meteo usage, off by default"). Under **Q15**, record the measured saving from the end-to-end test at `0.01` and `0.05` as input to the plan/interval decision.

## Part C: configuration and mappings
- **`config/config.go`:**
  - `period`: the weather poll interval, with the default changed from `1s` to **`1h`** (confirmed). At about 3,200 points × 2.7 calls that is about 6.2M Open-Meteo calls/month, above the Professional plan (5M), so this needs an Enterprise plan or a longer `period` (new rolling-plan question Q15).
  - **`LRFSites` (`lrf_sites`, list of LRF codes, default empty = all LRFs).** Limits tropobeat to the listed sites for limited runs and practical testing. Applied wherever LRFs are loaded (the start-up load and every discovery run), so it limits the whole pipeline: only those LRFs' demod channels are discovered, only their co-channel paths are built and published, and weather is fetched only for their LRF, midpoint and transmitter points. Codes match case-insensitively. Codes not in `lrf-coordinates` are logged as a warning at start-up. If none of the listed codes exist, the beat stops with a clear error rather than running with nothing. The run summary logs "limited to N of M LRFs". Example for a quick test: `-E 'tropobeat.lrf_sites=["TERH","PROV"]'`, or in `tropobeat.yml`: `lrf_sites: ["TERH", "PROV"]`.
  - `DemodIndex` (`demod_index`, default `log-metric-p-rfdemod-*`).
  - `FCCIndex` (`fcc_index`, default `fcc.DefaultIndex`).
  - `Cochannel{Schedule "0 3 * * *", Radius "200mi", DemodWindow 24h}` under `cochannel`.
  - Validation: the cron expression is checked by gocron at registration, and errors stop the beat at start-up.
- **`_meta/fields.yml`:** replace the template's `counter` field with the `tropobeat.cochannel.*` and `tropobeat.weather.*` fields above, using **`type: geo_point`** for every `location`, **`type: geo_shape`** for `tropobeat.cochannel.path`, `keyword` for IDs and call signs, `integer`/`float` for numbers, and `object` (never `nested`, which Kibana Maps can't use) for `levels` and the `lrf`/`midpoint`/`transmitter` sub-objects.
- **`_meta/config/beat.yml.tmpl` and `beat.reference.yml.tmpl`:** document the new settings, including a commented `#lrf_sites: ["TERH", "PROV"]` example explaining that it limits weather collection (and discovery) to those sites, and that empty means all. Note that the cron job for `insert fcctransmitters` should run before `cochannel.schedule` (e.g. 02:00 vs 03:00 UTC) so discovery reads a complete, freshly pruned index.
- **`mage update`** regenerates `tropobeat*.yml`, `fields.yml`, `docs/fields.asciidoc` and `include/fields.go`.

## Part D: `ROLLING_PLAN.md`
- **Phase 1:** mark the demod received-channel query, gocron scheduler and weather poll job ✅, with a note that the demod query and discovery run as one combined `cochannel` job (new Decisions row).
- **Open questions:**
  - **Add Q14:** "Seven LRFs use different codes in the demod telemetry than in `lrf-coordinates`: `BIRM`/`BIR`, `HTSBG`/`HSTBG`, `LUB`/`LUBB`, `PRESQ`/`PRSQ`, `RNO`/`RNC`, and `MON`/`MONR` are swapped (CSV `MON` = Monroe LA, demod `MON` = Monterey CA). Which codes are authoritative: fix the LRF list, fix the demod labels, or add a translation table in tropobeat? Until resolved, these sites are skipped by co-channel discovery. Also: `GLEN`, `GRNNC`, `VICT` and `WATER` had no demod data in the 2026-10-13 sample." Blocks: Phase 2 discovery for those 11 sites.
  - **Q13:** add the measured answer. 89% of locked channels match an FCC full-power station by call sign; 11% are LPTV/Class A/DTS/translator/foreign (no exclusion needed); 6 spare/test inputs are skipped. Narrow Q13 to whether tag hygiene (spacing, `(OTA)` variants) should be standardised at source.
  - **Add Q15**, a separate question for the API call budget: "Open-Meteo counts calls per location, and requests with more than 10 variables count as several calls. With about 3,200 weather points (211 LRFs, about 1,092 co-channel transmitters, about 1,879 path midpoints) at 27 variables, each poll costs about 8,600 calls. The hourly default is about 6.2M calls/month, above the Professional plan's 5M (every 3 h ≈ 2.1M; every 6 h ≈ 1.0M). Which plan will be purchased, and should we reduce calls by polling less often (one poll's hourly forecast covers the hours between), requesting fewer variables or pressure levels, or merging points that fall in the same weather-model grid cell?" Blocks: production weather polling.
  - **Q8/Q9:** leave them as they are, and reference Q15 for the numbers.
- **Known issues:** add the demod sample facts (about 1 report/min per input, 21% `Not Locked`).
- **Phase 1:** add a ✅ row for the `lrf_sites` setting: weather collection and discovery can be limited to chosen LRFs for limited runs and testing (empty = all). Under Q15, note that it is also a way to control API usage during trials.
- **Phase 2:** mark the discovery job, geodesic midpoint, path groups (in memory + metricset) and "weather poller reads path groups" ✅. The API call budget stays 📋, recording the point count from the end-to-end test.
- **Known issues:**
  - the FCC import cron must run before `cochannel.schedule`
  - avoid `insert … --recreate` while the beat is running
  - remove "the beat reads LRFs only at start-up": LRF locations are now re-fetched on every discovery run. Update the 2026-10-07 "LRF sites are loaded once at beat start-up" decision with a new Decisions row superseding it
- **Change log:** add an entry.

## Not doing
- The stale-data (3 h) weather fallback; it stays a Phase 1 📋 item.
- Ducting calculations (Phase 3).
- Committing (I'll offer).

## Verification
- **Unit tests:**
  - `ParseCallSign`: `WTHI / WTHI2 (OTA)` → `WTHI`; `CBS 10`, empty and `(OTA)` don't parse.
  - `Midpoint`: the README example ≈165,330 m, and the midpoint is equidistant from both ends within 1 m.
  - Exclusion: suffix-insensitive match, the nearest-hit fallback with a warning, and multiple tags on one channel.
  - `LoadReceived` and `Discover` against an `httptest` fake Elasticsearch covering composite paging, `Not Locked` filtering, unknown-ID skipping, the >150 mi sanity skip, spare-tag skipping and `_msearch`.
  - The gocron logger adapter; config defaults and unpacking; the `lrf_sites` filter (empty = all, case-insensitive, unknown codes warned, none-valid errors).
- `go build ./... && go vet ./... && go test ./...`, plus `go test ./libbeat/processors/ratelimit/` in `beats/` (the clockwork bump).
- **End to end against local Elasticsearch 8.17 (docker):**
  1. `insert lrfcoordinates`, then `insert fcctransmitters --states IN,IL,KY,OH,MI,RI,CT,MA`.
  2. **Preferred:** load the export in `sample/` (extract the tarball to the scratchpad; mapping, then data) into a local `log-metric-p-rfdemod-<date>` index with `npx elasticdump`, so the job runs against real demods for every LRF. Before that, run `insert fcctransmitters` for all states. Report:
     - tag consistency (Q13): the share of tags whose call sign matches an FCC record, and the unmatched tag patterns
     - `s_lrf_id` coverage against the 211 LRFs
     - channels per LRF and the demod sample rate
     - the total path and point count, for the API budget

     The export files stay outside the repo; a small anonymised excerpt may be committed later as a test fixture. **Fallback, or in addition:** seed `log-metric-p-rfdemod-2026.10.13` with sample-shaped docs: TERH RF 10 `WTHI / WTHI2 (OTA)`; PROV RF 25 `WJAR (OTA)` and RF 7 `WPRI`; a `CBS 12` tag (fallback path); an unknown `s_lrf_id`; and an `Unlocked` doc (ignored).
  3. **Grid-merge measurement:** with all LRFs, log the unique point count with `coordinate_precision` at `0`, `0.01` and `0.05` (one weather run each) and record the savings for Q15.
  4. **Limited run first:** start the beat with `-E 'tropobeat.lrf_sites=["TERH","PROV","BOS"]'` and check that only those LRFs' `cochannel` and `weather` events are published, and that the Open-Meteo point count is small (a few dozen). Then run the beat for all LRFs with `period: 1m` and `cochannel.schedule: "*/2 * * * *"`. Check that:
     - logs show discovery totals, the exclusion of WTHI-TV and WJAR, the fallback warning and the unknown LRF
     - `tropobeat-*` has `metricset.name:cochannel` events whose `lrf/midpoint/transmitter.location` fields are mapped as `geo_point` and `path` as `geo_shape` (`GET tropobeat-*/_mapping/field/tropobeat.cochannel.*`)
     - a `geo_shape` query (e.g. paths intersecting a box around Terre Haute) returns TERH's paths, confirming the lines are indexed as shapes Kibana Maps can draw
     - `weather` events exist for the paths and the LRF-only groups
     - while the beat is running, `insert lrfcoordinates` with one LRF's coordinates changed in a copy of the CSV is picked up by the next discovery run (its `cochannel` events and `lrf.location` change) without a restart
     - stopping the beat (SIGTERM) shuts the scheduler down cleanly
