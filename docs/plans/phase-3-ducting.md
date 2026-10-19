# Plan: Phase 3 — tropospheric ducting calculation (`internal/ducting`)

> **Status:** implemented in commit `ac12a5b` (2026-10-14). This is the working plan as approved, kept for reference. For how the finished code behaves, see [DUCTING.md](../../DUCTING.md); for project status and open questions, see [ROLLING_PLAN.md](../../ROLLING_PLAN.md).
>
> **Where the build differs from this plan:**
> - **Trapping formula (§6).** The candidate `λ_max = 2.5 · d · √(ΔM · 10⁻⁶)` couldn't be confirmed from a source, and a published comparison of trapping methods questions that approach. The code uses the US Naval Postgraduate School's rule instead: `f_min = 360,000 / d^1.5` MHz (d = duct thickness in m), which depends on thickness only.
> - **Worked example (§7, readme §6).** With that rule, TERH's 228 m surface duct traps everything above about 105 MHz, so RF 10 (195 MHz) is `trapped: true`, not `false`. The channel table in the readme lists the thinnest duct per channel instead of values at ΔM = 10.
> - **Limited run.** Run on 2026-10-14 against Elasticsearch 9.4.2 (TERH, PROV, BOS): 49 weather and 80 ducting events; every `profile_id` link and `path_ids` filter checked out. The radiosonde soundings comparison and the Kibana Maps check were not done (rolling plan Q16, Q17).

## Context
Phases 1–2 deliver, every hour, an atmospheric profile at the LRF, midpoint and transmitter of each co-channel path (surface 2 m + 8 pressure levels, 1000–700 hPa: temperature K, RH %, geopotential height m ASL; terrain elevation). Phase 3 turns each profile into refractivity, layer gradients, duct detection and a likelihood score, then combines the three points into a **path ducting index** that Phase 5 will correlate with MER/BER.

The user supplied the core formulas and asked for a deeper check that they are correct. Decisions taken: **implement the user's formulas exactly, with the full ITU-R P.453-14 forms as a test cross-check**; **put a summary (path index, per-point score and class) on the existing `weather` events, and the detailed profile (N/M per height, layer gradients, duct geometry) in a new `ducting` metricset**, one event per unique location.

Existing code to build on:
- `openmeteo.Profile` / `Level` (`internal/openmeteo/types.go`): `Elevation`, `Surface` (HeightM = Elevation + 2), `Levels` in increasing height, all values `*float64` (nil = missing).
- `weatherEvent` / `profileFields` / `levelFields` (`beater/events.go`): where results are added to events.
- `cochannel.Path.RFChannel` (`internal/cochannel/path.go`): the channel for frequency-dependent trapping.
- Fixture `internal/openmeteo/testdata/denver.json`: real high-terrain case with underground levels.

## Investigation: are the formulas right?

**1. Vapour pressure** `e = RH/100 · 6.11 · 10^(7.5·t/(t+237.3))`, t = T − 273.15 °C, e in hPa.
- Magnus–Tetens fit over **liquid water**. Correct; T must be in K on input (our data is K).
- ITU-R P.453-14 form: `e_s = EF · 6.1121 · exp((18.678 − t/234.5) · t/(t+257.14))`, `EF = 1 + 10⁻⁴(7.2 + P(0.0320 + 5.9·10⁻⁶ t²))`. The difference is mostly the enhancement factor EF (≈ +0.4 %).
- Worked: 15 °C, 60 %, 1013.25 hPa → e = 10.23 hPa (ITU 10.27). 30 °C, 90 %, 1000 hPa → e = 38.20 (ITU 38.38).
- Below 0 °C: keep the water form. Model RH is reported relative to water (verify for the Open-Meteo models in use, Q10); using the ice form with a water-referenced RH would be wrong. Cold levels hold little vapour, so the effect on N is small either way.

**2. Refractivity** `N = 77.6/T · (P + 4810·e/T)`.
- This is the Smith–Weintraub form, which ITU-R P.453 itself gives as the approximation for radio frequencies (within ~0.02 % of the full expression). P is **total** pressure in hPa: the level value (1000, 975 …) for pressure levels, `surface_pressure` for the surface.
- Worked: 15 °C, 60 %, 1013.25 hPa → **N = 318.9** (standard-atmosphere sea-level N is ~315; consistent).
- Sensitivity: ∂N/∂e ≈ 4.5 N/hPa at 288 K, so the formula choice in step 1 moves N by 0.2 N (15 °C case) to 0.7 N (30 °C humid case). Under 1 N, against duct gradients of ≥ 16 N per 100 m. **Formula choice is not the accuracy limit; vertical resolution is** (below).

**3. Modified refractivity** `M = N + 0.157·h`, duct condition `dM/dh < 0`.
- 0.157 = 10⁶ / Earth radius (6,371 km), per metre. h in metres. Correct.
- `dM/dh < 0` ⇔ `dN/dh < −157 N/km`: the trapping (ducting) condition. Correct.
- The constant offset of h cancels in gradients, so geopotential height ASL can be used directly; duct heights are **reported above ground** (h − Elevation). Geopotential vs geometric height differs < 0.1 % below 3 km: negligible.
- ITU-R P.453 classes on dN/dh (N/km): **sub-refractive > 0**, **normal 0 to −79**, **super-refractive −79 to −157**, **ducting < −157**.

**4. What actually limits accuracy — vertical resolution.**
- Level spacing is ~210 m near 1000 hPa, ~500 m at 850–800, ~1,000 m at 800–700. Real ducts are often 20–200 m thick.
- A gradient over a coarse layer is an average: a 50 m duct at −400 N/km inside a 200 m layer that is otherwise −40 N/km gives (−400·50 − 40·150)/200 = **−130 N/km**, i.e. "super-refractive", and the duct is missed.
- Consequences for the design: (a) score super-refractive layers too, not just `dM/dh < 0`; (b) give the **surface→first-level layer** weight, since surface-based ducts (night-time inversions, coastal moisture) form there; (c) treat outputs as a likelihood, to be calibrated against soundings (Verification) and incidents (Q7).
- Underground levels: Open-Meteo extrapolates levels below terrain (Denver: 1000–850 hPa). Drop any level whose height ≤ the surface point (Elevation + 2 m). Also drop a level less than **20 m** above the previous kept point, since a tiny dz turns rounding noise into huge gradients.

**5. Duct geometry (from the M profile).**
- Trapping layer = consecutive layers with dM/dh < 0. ΔM (duct strength) = M at its base − M at its top.
- Duct type (ITU-R P.453 definitions): **surface duct** if the trapping layer starts at the surface point; **surface-based duct** if it is elevated but M_top ≤ M_surface (the duct reaches the ground); otherwise **elevated duct**, whose base is where M below the layer first rises back to M_top (linear interpolation).
- Report base, top, thickness (m AGL) and ΔM.

**6. Frequency-dependent trapping.**
- A duct traps a wavelength only if it is thick enough. Candidate (Kerr 1951, widely quoted): `λ_max = 2.5 · d · √(ΔM · 10⁻⁶)` (λ, d in m). **Verify against the source before coding.**
- Illustration at ΔM = 10: RF 14 (473 MHz, λ 0.63 m) needs d ≥ ~80 m; RF 7 (177 MHz) ≥ ~210 m; RF 2 (57 MHz) ≥ ~670 m. This is why low-VHF needs much thicker ducts than UHF.
- Channel → centre frequency (US, post-repack): RF 2–4 = 57 + 6(n−2); RF 5–6 = 79 + 6(n−5); RF 7–13 = 177 + 6(n−7); RF 14–36 = 473 + 6(n−14) MHz.

**7. Score and path index (initial, to be tuned in Phase 5).**
- Point score 0–1 from g = the most negative layer dN/dh (N/km), linear within each band:
  - g ≥ −79 → 0
  - −157 ≤ g < −79 → 0.6 · (−79 − g) / 78 (super-refractive, capped at 0.6 so a true duct always scores higher)
  - −300 ≤ g < −157 → 0.6 + 0.4 · (−157 − g) / 143
  - g < −300 → 1.0
  - Band edges (−79, −157, −300) and the 0.6 split in config, so they can be calibrated.
- **Path index = plain mean of the three point scores** (user decision): `(score_lrf + score_midpoint + score_transmitter) / 3`. Frequency is not part of the index.
- **`trapped`** (separate boolean) = any duct at any of the three points has λ_max ≥ the channel's wavelength (§6). Phase 5 combines index and trapped.
- Worked: LRF g = −163.9 → 0.62; midpoint g = −112.4 → 0.26; transmitter g = −41.2 → 0; index = 0.29. LRF surface duct 228 m, ΔM 1.6 → λ_max ≈ 0.71 m (≈ 420 MHz); RF 10 = 195 MHz → `trapped: false`.
- Known limits, recorded in the rolling plan for Phase 5 tuning: the index ignores frequency (a UHF-only duct raises a VHF path's index), and the mean doesn't distinguish one strong point from three moderate ones.

## Implementation

**New package `internal/ducting`** (pure functions, no I/O; imports `openmeteo` types only)

| File | Contents |
|---|---|
| `refractivity.go` | `VapourPressure(tK, rhPct)`, `Refractivity(pHPa, tK, eHPa)`, `Modified(n, hM)` — exactly the user's formulas, documented with units and the ITU-R P.453 reference |
| `profile.go` | `FromProfile(openmeteo.Profile) (Profile, error)`: surface + above-ground levels (rules in §4), each point with h ASL/AGL, N, M; `Layers()` with dN/dh, dM/dh (per km) and `Class` |
| `duct.go` | `Ducts(Profile) []Duct` (type, base/top/thickness AGL, ΔM) per §5 |
| `score.go` | `Score`, `ChannelMHz(rf)`, `Traps(duct, mhz)`, `PathIndex(points, rf)` per §6–7; thresholds in a `Config` with defaults |
| `itu_test.go` | Cross-check against the full ITU-R P.453-14 forms (written in the test only) |

**Events (`beater/events.go`, `beater/jobs.go`)** — summary on `weather`, detail in a new `ducting` metricset.

`weather` events (one per path or LRF-only group, as today) get only the summary:
- Path level: `tropobeat.weather.ducting.{index, frequency_mhz, trapped}`.
- Each point (`lrf`/`midpoint`/`transmitter`): `ducting.{score, class, min_dndh_n_per_km, profile_id}`. `profile_id` links to that point's `ducting` event.
- The raw weather (`surface`, `levels[]`) stays as it is now; no N/M values are added to it.

New `ducting` metricset (`metricsetDucting = "ducting"`), **one event per unique weather location per poll**, not per path point: an LRF shared by many paths is published once. On the full network that is ~2,589 events per poll instead of ~5,512.
- `@timestamp` = fetch time (same as the `weather` events of that poll); `forecast_time`; `run_id`.
- `profile_id` = the requested coordinate as a string (`openmeteo.Coordinate.String()`, already rounded by `coordinate_precision` when set), so it equals the `profile_id` on the weather points that used it. `location` (geo_point), `elevation_m`.
- `points[]`: surface + each kept level, `{pressure_hpa, height_m, height_agl_m, temperature_k, relative_humidity_pct, vapour_pressure_hpa, refractivity_n, modified_refractivity_m}`.
- `layers[]`: `{base_agl_m, top_agl_m, dndh_n_per_km, dmdh_m_per_km, class}`.
- `ducts[]`: `{type, base_agl_m, top_agl_m, thickness_m, delta_m}`; empty when none.
- `dropped_levels`: count of levels removed as underground or too close (§4).
- `path_ids` and `lrf_codes` (keyword arrays, sorted): every path and LRF in this poll that uses this location (an LRF-only group contributes its LRF code, no path ID). Collected in `runWeather` while de-duplicating by `profile_id`. This lets one Kibana filter (`path_ids: "TERH:1001:10"`) show a path's three ducting docs. Average ~9 paths per LRF, so the arrays stay short.
- Same summary as the weather points (`score`, `class`, `min_dndh_n_per_km`), so the detail events can be filtered on their own.
- Built in `runWeather` from the unique point profiles of the `GroupProfiles` results (de-duplicated by `profile_id`) and published in the same `PublishAll` call. A new `ductingEvent(...)` sits next to `weatherEvent` in `events.go`.
- A location with fewer than 2 usable points gets a `ducting` event with `class: insufficient_data` and no score, and its weather points get no `ducting` summary. The count is logged per run.
- **What gets published is configurable**: `ducting.publish: all | notable` (default `all`).
  - `all`: every unique location every poll, including `normal`, `sub_refractive` and `insufficient_data` (~62,000 docs/day on the full network). No ducting is stated explicitly (`class: normal`, `score: 0`, `ducts: []`), so a missing doc only means the weather fetch failed. This is the default during calibration, because normal-day profiles are the baseline for the soundings comparison and Phase 4.
  - `notable`: only `super_refractive` and `ducting` locations. The weather summaries are unchanged and still give every point's score and class.
  - Validated at start-up (unknown value → error). The poll log line reports `N ducting events (M notable)` in both modes.
- The run's log line adds `N ducting events`.

**Config / mappings / docs**
- `config/config.go`: optional `ducting:` section (`publish: all|notable`, score thresholds, min layer thickness) with defaults; templates in `_meta/config/*.tmpl`.
- `_meta/fields.yml`: the summary fields under `tropobeat.weather` (`float`, `keyword` for class and `profile_id`, `boolean` for trapped), and a new `tropobeat.ducting` group (`location` geo_point; `points`, `layers`, `ducts` as `object`, not `nested`, for Kibana). Arrays of objects are flattened, so queries across one element's fields aren't exact; normal queries use the summary fields. `make update`, then restore the local `tropobeat.yml` (it overwrote local settings last time).
**`ROLLING_PLAN.md`** (customer-facing style: behaviour, status and decisions; brief library/package names OK; no versions, function names or tooling)
- **Header:** "Current phase" → Phase 3 in progress (✅ when done); "Last updated" date.
- **Architecture notes:** one bullet saying ducting results are published as a summary on `weather` events and in detail as the new `ducting` metricset, described in `DUCTING.md`.
- **Phase 3 section:** an intro line linking to [DUCTING.md](DUCTING.md) as the technical reference for classes, scoring and fields. Rows updated or added, each ✅ when built:
  - Above-ground profile (underground and too-close levels dropped)
  - Refractivity N and modified refractivity M (the agreed formulas; checked against ITU-R P.453 to within about 1 N-unit)
  - Layer gradients and classes (sub-refractive, normal, super-refractive, ducting)
  - Duct properties (surface, surface-based, elevated; base, top, thickness, strength)
  - Ducting likelihood score (bands and initial thresholds)
  - Frequency-dependent trapping (`trapped`)
  - Path ducting index (plain mean of the three point scores)
  - Published results: summary on `weather` events; `ducting` metricset, one event per location per poll with the paths and LRFs that use it; `ducting.publish` all/notable
  - Calibration against radiosonde soundings: 📋 until the research step is done, then the findings
  - Technical readme `DUCTING.md`
- **New open questions** (numbering continues after Q15):
  - **Q16 Calibration:** the score thresholds (−79 / −157 / −300 N/km and the 0.6 split) are initial values from ITU classes. Which ducting events and soundings should they be calibrated against, and who signs off the values? Links to Q7. Blocks: Phase 5 detection rules.
  - **Q17 Vertical resolution:** model pressure levels are 200–1,000 m apart, so ducts thinner than a layer are under-scored or missed. Is extra weather detail near the ground (more levels or a higher-resolution model) worth the added API usage? Links to Q10, Q15. Blocks: Phase 3 accuracy.
  - **Q18 Path index method:** the index is a plain mean, so it ignores the channel frequency and where along the path the duct sits. Should Phase 5 move to a frequency-aware or midpoint-weighted index? Blocks: Phase 5 scoring.
  - **Q19 Ducting data volume:** `publish: all` keeps about 62,000 ducting documents a day. When should production switch to `notable`, and what retention suits the ducting data? Links to Q15. Blocks: Phase 8 deployment.
  - Q10 gains a note: confirm the weather models report humidity relative to water at all temperatures, as the vapour-pressure formula assumes.
- **Decisions** (dated): the agreed formulas with ITU-R P.453 as a cross-check; summary on `weather` events plus a separate `ducting` metricset, one event per location; the plain-mean path index with `trapped` kept separate; `ducting.publish` defaults to `all` during calibration; ducting events list the paths and LRFs that use them so maps and searches can link them.
- **Known issues:** vertical resolution (the smoothing example in plain words); thresholds not yet calibrated (Q16).
- **Change log:** one entry for Phase 3.

**Technical readme: `tropobeat/DUCTING.md`** (project root, next to `ROLLING_PLAN.md`)

Written last, from the finished code, so every number and field name matches what is published. Audience: engineers and operators reading ducting data in Kibana; plain language, short sections, tables over prose, no Go code.

Sections:
1. **What ducting is, in one paragraph**: why a duct carries a co-channel signal past the horizon, and what tropobeat measures.
2. **How a profile is built**: the surface point plus pressure levels; why underground and too-close levels are dropped (`dropped_levels`).
3. **The calculation in three steps**: vapour pressure → refractivity N → modified refractivity M, each formula with units and a one-line meaning, plus the worked 15 °C example (e = 10.23 hPa, N = 318.9).
4. **Classes**: a table of `sub_refractive` / `normal` / `super_refractive` / `ducting` / `insufficient_data`, with the dN/dh and dM/dh range of each, what the radio signal does, and what it means for co-channel interference.
5. **Duct types**: a table of `surface` / `surface_based` / `elevated`, each with a small text sketch of the M profile.
6. **Scoring**: the point score bands in a table, the path index (plain mean of three scores), `trapped` and the trapping formula, and a channel → minimum duct thickness table (e.g. RF 2, 7, 14, 36 at ΔM = 10). The TERH worked example (scores 0.62 / 0.26 / 0 → index 0.29, `trapped: false`).
7. **Field reference for the `ducting` event**: one table per part (`event-level`, `points[]`, `layers[]`, `ducts[]`) with columns *Field · Unit · Meaning · How to read it* (e.g. `delta_m`: "M-units; duct strength; under 5 is weak, traps only UHF unless the duct is thick"). Plus the summary fields on `weather` events and how `profile_id` links the two.
8. **How to interpret results** (including what a missing ducting doc means under `publish: all` vs `notable`): a short "what to look for" table (e.g. high index + `trapped` true → likely interference; `ducting` class but `trapped` false → duct too thin for this channel; `insufficient_data` → mountain site or missing data).
9. **Kibana Maps setup**: a ducting layer on `tropobeat.ducting.location` using "top hits per entity" (entity `profile_id`, sort `@timestamp`, 1 hit) so each location shows its latest doc; tooltip fields limited to the summary (`class`, `score`, `min_dndh_n_per_km`, duct thickness and ΔM); colouring the weather `path` layer by `ducting.index`; filtering on `path_ids` to jump from a path to its three ducting docs; a dashboard drilldown from the map to Discover filtered on the clicked `path_id` (check it works on the deployed Kibana version first).
10. **Limitations**: vertical resolution (the −130 N/km smoothing example), model RH and the water-only vapour formula, initial thresholds still to be calibrated.

`ROLLING_PLAN.md` links to it from the Phase 3 section; `DUCTING.md` links back to the rolling plan's open questions (Q16–Q19) in its Limitations section.

## Verification
- **Unit tests (hand-computed):** e = 10.23 hPa and N = 318.9 at 15 °C/60 %/1013.25 hPa; M = N + 0.157 h; dM/dh = 0 exactly at dN/dh = −157 N/km; class boundaries at −79 and −157.
- **ITU cross-check:** user vs full P.453-14 forms differ by < 1.5 N over −40…35 °C, 0–100 % RH, 700–1050 hPa.
- **Synthetic profiles:** standard atmosphere (no duct, class normal); a surface inversion with a moisture drop (surface duct); an elevated trapping layer above and below M_top (elevated vs surface-based); the −130 N/km smoothing case (super-refractive, score 0.3–0.6).
- **Denver fixture:** underground levels dropped; remaining layers classified.
- **Against real soundings (research step):** pick 2–3 radiosonde stations near LRFs and dates with known ducting (Gulf coast, Great Lakes, Southern California). Compute the M profile from the full-resolution sounding (University of Wyoming upper-air archive) and from Open-Meteo's historical forecast at the same hour; record whether the coarse levels detect, downgrade or miss each duct. Results go into the rolling plan as the calibration baseline.
- **Publish modes:** unit test over a mixed set of locations: `all` publishes every one; `notable` publishes only `super_refractive`/`ducting`, while the weather summaries are identical in both modes; an unknown value fails config validation.
- **Rolling plan check:** Phase 3 rows match what was built; Q16–Q19 added with no reused numbers; the link to `DUCTING.md` resolves; no function names, versions or tooling in the new text.
- **Readme check:** every field in the `_meta/fields.yml` `tropobeat.ducting` group appears in the `DUCTING.md` field tables, and the worked numbers in the readme match the unit tests.
- **End to end:** limited run (`lrf_sites: ["TERH", "PROV", "BOS"]`), check `weather` events carry the path `index` and per-point summaries; `ducting` events exist, one per unique location (count matches the "unique after merge" figure in the poll log), and every weather point's `profile_id` finds exactly one `ducting` event from the same poll; filtering ducting events on one `path_ids` value returns exactly three docs (two for a path whose midpoint merged with an end point under `coordinate_precision`); in Kibana Maps, the ducting layer with top hits per `profile_id` shows one dot per location; mappings are correct (`_mapping/field/tropobeat.ducting.*`, `tropobeat.weather.*.ducting.*`), and `go build/vet/test ./...` pass.

## Not doing in Phase 3
- Correlation with telemetry (Phase 5) and forecast-hour scoring across `forecast_days` (Phase 6).
- Fetching extra height levels (e.g. 80/120/180 m); revisit if the soundings comparison shows the surface layer is too coarse.
