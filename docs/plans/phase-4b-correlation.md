# Plan: Phase 4b — CCI correlation engine (A/B pairing + co-channel ducting)

> **Status:** implemented on the `phase-4b-correlation` branch (2026-10-21). This is the working plan as approved, kept for reference. For how the finished code behaves, see [CORRELATION.md](../../CORRELATION.md) and [README.md](../../README.md); for project status and open questions, see [ROLLING_PLAN.md](../../ROLLING_PLAN.md).
>
> **Where the build differs from this plan:**
> - **One-sided verdicts wait (`pair_grace`, 2m).** Without it, every A+B episode started and ended with a spurious `single_side` minute while the second side's own 3-report hysteresis caught up.
> - **Outbox instead of a blocking event channel.** Events and trace records wait in the engine's own queue until the beater takes them, so the beater blocked in `Submit` can never deadlock the engine. Events are never dropped; traces beyond `trace.buffer` are.
> - **Paths with no usable weather count as unknown.** Found while designing the tests: an incomplete weather fetch left those paths out of the ducting store, so their channel looked as if it had no co-channel paths (`dual_unexplained`). They are now kept with an empty state, and `dual_ducting_unknown` (reason `ducting_incomplete`) results when no known path is ducting. The mock gained a `no_data` scenario to reproduce it.
> - **Pair reason codes.** `other_side_missing` replaces `unpaired_no_side_b`/`other_side_unknown`; `ducting_no_poll`, `ducting_stale` and `ducting_incomplete` replace `ducting_unknown`. The ducting category adds `path_no_data`.
> - **`correlator.enabled`** was added; **`trace.categories`** is a list type that replaces the default list as a whole (the configuration library otherwise merged `[input]` into `[pair, hysteresis]` element by element, and `[]` could not turn tracing off).
> - **Baselines go stale as a skip reason** (`baseline_stale`) rather than silently missing.
> - **The 4th test LRF is IND** (mock `no_data`) for the ducting-unknown step, instead of a mock failure window lined up with the scenario; `fail_every`/`fail_for` repeat on the clock and are optional.
> - **`-quiet` uses each input's best sample MER**, not its median: the 15-minute sample's median can sit on the drop threshold of the multi-day baseline.
> - **Channels in the scenarios** come from the test cluster's latest discovery run: TERH RF 10 has no co-channel paths, so the TERH CCI steps use RF 31, 35, 18 and 23.
> - **Saved search** is placed on the dashboard as a panel, because `tropobeat setup` only installs dashboards and the objects they reference.
> - **Recovering sides (found in the end-to-end runs).** A side whose degraded inputs are all reporting clean again, but haven't had 3 clean reports yet, is `recovering`. It keeps going a verdict that side counted towards, but never starts one, never joins one it didn't count towards, and never changes the verdict: when only recovering sides count, the verdict holds until it clears. Without this, a frame coming back after an outage raised 0-second `single_side` episodes, a returning frame turned the other side's `unpaired` (`guard_frame_down`) into `cci_confirmed`, and a one-sided verdict was published a second before the pair cleared.
> - **Events carry the held reason.** While a change waits for `pair_grace`, `ongoing` events carry the reason of the verdict in effect, not of the one waiting.

## Context

Phase 4a is built: live demod reports arrive from RabbitMQ and are only counted in `consume()` ([beater/tropobeat.go](tropobeat/beater/tropobeat.go)). Each input also has a 7-day baseline in `baselineStore` ([beater/baseline.go](tropobeat/beater/baseline.go)), and each co-channel path's latest ducting result sits in `ductingStore` ([beater/ductingcache.go](tropobeat/beater/ductingcache.go)), indexed by (LRF, RF channel). Phase 4b adds the engine that turns these into a root-cause verdict.

1. For each demod input, detect BER and MER stress against its own baseline.
2. Pair the A and B demods of the same LRF and RF channel. Both sides share one antenna feed, so when both degrade together the problem is in the received RF, not in one demod.
3. Match that dual-side degradation to the ducting status of the co-channel paths on that channel. Ducting on a path is the environmental evidence that names CCI, and the interfering transmitter, as the root cause.

**User decisions (this session):**
- **Ducting gate.** dM/dh < 0 (path state `ducting` or `trapping`) on any co-channel path on the channel. A duct that traps the channel's frequency is reported as stronger evidence, and the strongest path is named.
- **Lock loss.** A channel losing lock counts as an anomaly, alongside BER spike and MER drop. This applies to one channel only, not a whole frame or site.
- **Output.** 4b publishes a `correlation` metricset event on every pair state change. 4c keeps periodic snapshots, alerts and the inSITE integration.
- **Scope.** The CCI rule only. Full outage exclusion and frame-loss rules are deferred. A minimal guard (below) stops whole-frame or whole-site loss from being read as CCI.

This supersedes the 4b sketch in [docs/plans/phase-4-rf-telemetry.md](tropobeat/docs/plans/phase-4-rf-telemetry.md#phase-4b-correlation-engine-designed-now-built-next-session) where they differ. The package moves to `internal/`, following the 4a convention set for `rabbitmqclient`.

## 1. Engine package: `internal/correlator` (new)

Modelled on routelogbeat's `beater/correlator` (`~/routelogbeat_workspace/routelogbeat/beater/correlator/correlator.go`, `clock.go`): one actor goroutine owns all state, so no locks are needed.

```
run():
  ticker = clock.NewTicker(SweepInterval)
  for select:
    <-closed        -> return
    r := <-in       -> ingest(r): update instance state, re-evaluate its (LRF, channel) pair
    now := <-ticker -> sweep(now): expire silent instances, re-evaluate affected pairs
                       (sides going stale, ducting refreshed or gone stale)
```

**API** (mirrors routelogbeat):
- `New(cfg Config, opts ...Option) (*Engine, error)`
- `Submit(demod.Report) bool` (blocking) and `TrySubmit`
- `Events() <-chan Event`
- `Stats() Stats` (atomic counters)
- `Close()` (idempotent)

**Options:** `WithBaselines(Baselines)`, `WithDucting(Ducting)`, `WithClock(Clock)`. Copy `clock.go` from routelogbeat: `Clock`, `Ticker`, `RealClock`.

**Injected sources.** These interfaces are defined in `correlator` with its own small value types, so it never imports `beater`:
- `Baselines.Lookup(demod.Instance, rf int) (Baseline, bool)`. `Baseline` holds `EffectiveBER`, `MedianMER`, `Usable`, `RunID`.
- `Ducting.Channel(lrf string, rf int, at time.Time) (ChannelDucting, known bool)`.
  - `ChannelDucting` holds `PolledAt`, `ForecastTime` and `Paths []PathEvidence`.
  - Each `PathEvidence` holds `PathID`, `CallSign`, `FacilityID`, `DistanceMi`, `State ducting.State`, and the labels of the points with dM/dh < 0.
  - `Strongest()` returns the path with the highest `State.Level()`, ties broken by the shortest distance.

**State owned by the actor:**
- `map[demod.Instance]*instanceState`.
- `map[demod.ChannelKey]*pairState`.
- `map[string /*host IP*/]map[demod.Instance]struct{}`, used for the frame guard.

### Per-input rules (on each report, against its usable baseline)

Inputs without a usable baseline are ignored and counted in `Stats.NoBaseline`.

| Condition | Rule (thresholds are config) |
|---|---|
| `ber_spike` | Locked, and BER ≥ `ber_factor` (10) × EffectiveBER, or the reading is saturated (`demod.IsSaturatedBER`). |
| `mer_drop` | Locked, and MER ≤ MedianMER − `mer_drop_db` (3). |
| `lock_lost` | Not locked, while the input has an `ok` baseline (it is normally locked). |
| Environmentally Stressed | `ber_spike` held for `hysteresis_cycles` (3) consecutive reports. Kept as its own flag, matching the original rule. |

**Hysteresis.** Each report is one cycle for that input.
- An input becomes **degraded** after 3 consecutive anomalous reports (any of the three conditions).
- It clears after 3 consecutive clean reports.
- A missing minute counts neither way. A gap longer than `expire_after` (3m) resets both counters, and the sweep drops the input's live view.

**Signature (supporting evidence only).** Each input keeps a small ring of recent pre-onset power readings.
- At onset, power is compared with the pre-onset median and classed as `steady_or_rising` (typical of CCI) or `dropping` (a fade).
- The class is carried in the event and is never a gate.

### Pair rules (per LRF and RF channel; re-evaluated on each ingest for the channel and on each sweep)

**Side view.** Only instances with a fresh report (≤ `stale_after`, 90s) count.
- A side is degraded if any of its fresh instances is degraded. This covers the 11 channels received twice on one frame.
- A side is clean if all of its fresh instances are clean.
- A side is unknown if it has no fresh instance.
- Instances with no side label are left out of pairing (Q23).

**Lock-loss guard** (a stand-in for the deferred outage and frame rules). A side whose only anomaly is `lock_lost` counts as degraded only if another input on the same frame (host IP) has a fresh locked report. A whole frame down, or a whole site down, therefore never pairs into CCI.

**Pair state**, with the ducting looked up at evaluation time:

| State | Meaning |
|---|---|
| `clear` | No side degraded. |
| `single_side` | One side degraded, the other clean. Points to a demod or equipment fault, not CCI. |
| `unpaired` | Degraded, but the channel has only one side at this LRF, or the other side is unknown. CCI can't be confirmed. |
| `dual_ducting_unknown` | Both sides degraded, but there are no fresh ducting results (no poll yet, or older than 2 × period). |
| `dual_unexplained` | Both sides degraded, ducting is known, and no co-channel path on the channel has dM/dh < 0 (this includes a channel with no co-channel paths). The received signal is degraded, but there's no environmental evidence. |
| `cci_confirmed` | Both sides degraded, and at least one co-channel path on the channel is at `ducting` or `trapping`. Evidence strength is `trapping` or `ducting`, from the strongest path. |

**Events.**
- An `Event` is emitted on every pair state change.
- An `update` event is emitted when `cci_confirmed` evidence changes: the strength moves between ducting and trapping, or the strongest path changes.
- Each state run gets an episode ID. On clear, the event carries the episode's duration and peak state.
- **Ongoing events (user, 2026-10-21).** While a pair is not `clear`, the sweep emits an `ongoing` event every `snapshot_interval` (default 5m; 0 turns it off), measured from the last event for that pair. It carries the same episode ID, the current state and evidence, and the elapsed duration. A dashboard with a short time range therefore still sees an active episode. Clear pairs never repeat.

### `Event` contents

- Kind (start, update, ongoing, clear), time, LRF, RF channel, state, previous state, episode ID, and the onset time of each side.
- Per side: its instances (ID, frame and card labels), the conditions active, the latest BER, MER and power, the baseline's effective BER and median MER, and the signature.
- Ducting: known, poll time, forecast hour, strongest path (ID, call sign, facility ID, distance, state), and every path at ducting or trapping.

**Config** (`correlator.Config`, defaults applied as in routelogbeat): `SweepInterval` 10s, `StaleAfter` 90s, `ExpireAfter` 3m, `InputBuffer` 1024, `BERFactor` 10, `MERDropDB` 3, `HysteresisCycles` 3, `LockLoss` true, `SnapshotInterval` 5m.

## 1b. Decision trace: published, searchable debug records

**User decisions (2026-10-21).**
- The engine's reasoning (hits, misses and why) is published to the tropobeat index, so it can be searched and debugged from a dashboard.
- The trace is split into categories that can be switched on individually.
- `pair` and `hysteresis` are on by default.
- A debug dashboard ships with 4b.

**Pattern.** Taken from routelogbeat's `WithUnresolvedObserver` and `unresolvedHint` (`~/routelogbeat_workspace/routelogbeat/beater/correlator/debug.go`, `beater/correlator_debug.go`).
- The engine builds a trace record only when its category is enabled. A disabled category costs one bitmask check, and no diagnostics are computed.
- Records are delivered on a buffered `Traces() <-chan Trace` with a non-blocking send. When the buffer is full the record is dropped and `Stats.TraceDropped` goes up, so tracing can never stall the actor or hold back acks.

**Categories** (`correlator.trace.categories`; the default is `[pair, hysteresis]`):

| Category | Emitted when | Typical `reason` codes | Volume |
|---|---|---|---|
| `input` | Every report is evaluated against its baseline: values, baseline, conditions met or not, counter values. | `ok`, `ber_spike`, `ber_saturated`, `mer_drop`, `lock_lost` | ~2,800/min on the full network; use with `trace.lrf_sites` |
| `skip` | A report is not evaluated. Emitted once per instance per hour, not on every report. | `no_baseline`, `baseline_insufficient`, `baseline_invalid`, `baseline_no_station`, `baselines_stale`, `no_side` | Low |
| `hysteresis` | An input's counters cross a threshold or reset. | `degraded_enter`, `degraded_clear`, `stressed_enter`, `stressed_clear`, `gap_reset`, `expired` | Low |
| `pair` | A pair evaluation ends in a non-clear state, or changes state. Records the side views, the guard result, the ducting summary, the chosen state, and which rule decided it. | `cci_confirmed`, `single_side`, `unpaired_no_side_b`, `other_side_unknown`, `other_side_stale`, `ducting_unknown`, `ducting_none_on_channel`, `no_cochannel_paths`, `guard_frame_down` | Low to moderate |
| `ducting` | A degraded pair looks up ducting: every co-channel path on the channel with its state and points, which path was chosen as strongest, and the poll age. | `path_trapping`, `path_ducting`, `path_below_ducting`, `poll_stale`, `no_poll` | Low |
| `sweep` | A summary per sweep: instances expired, sides gone stale, pairs re-evaluated, trace drops. | `summary` | 6/min |

**Trace record fields** (metricset `correlation_trace`, under `tropobeat.correlation_trace.*`):
- `category`, `reason` (a keyword code from the table), `decision` (`hit`, `miss` or `skip`), and `message` (a one-line hint built like `unresolvedHint`, e.g. "side B has no fresh report (last 2m14s ago); pair can't be confirmed").
- `episode_id`, linking the record to its `correlation` events, plus the instance ID, side, frame and card labels.
- The values and thresholds used: BER, effective BER, the BER threshold, MER, median MER, the MER threshold, power, and the counters.
- The ducting detail.
- The shared identifying fields (`keys` helper: LRF, channel, band, path IDs, transmitters) and `lrf.location` (`geo_point`), so the Ducting Watch filters apply to these records too.

**Config** (`correlator.trace`):
- `categories`.
- `lrf_sites` (an empty list traces every LRF; a list limits tracing to those LRFs, which is the way to use `input` safely).
- `buffer` (4096).
- `log` (default false; also writes each record to the tropobeat log via `logp` debug, selector `correlator`, as routelogbeat does).
- An unknown category name stops start-up with a clear message.

**Debug dashboard: "Correlation debug"**, shipped in `_meta/kibana` and loaded by the standard dashboard setup, like Ducting Watch.
- LRF, channel, category and reason controls, plus a time slider.
- Reasons over time, stacked by `reason`. This is where misses show up (e.g. a spike of `other_side_stale`).
- The pair decision table: the latest `pair` record per LRF and channel, with state, reason, the A and B conditions and the strongest path.
- For a picked pair, the A and B BER and MER against their thresholds over time (from `input` records when enabled), with `correlation` start, ongoing and clear events marked.
- A saved search of trace records, with category, reason, message, LRF, channel, side and episode columns.
- Built on the test Kibana as **a new copy, without overwriting existing objects**, then exported into the repo. A section is added to DASHBOARD.md.

## 2. Beater changes

- **[beater/ductingcache.go](tropobeat/beater/ductingcache.go).**
  - Extend `pathDucting` with `State ducting.State`, the transmitter's call sign and facility ID, `DistanceMi`, and the labels of the points with dM/dh < 0.
  - In `pathDuctingFrom`, compute the state with `ducting.StateOf` over the path's analyses and the channel's frequency. Reuse the same channel → MHz helper that `weatherEvent` and `ductingSummary` use for trapping (in [beater/events.go](tropobeat/beater/events.go) / `internal/ducting`). `fg.path` already carries the transmitter and distance.
  - Add `Channel(lrf, rf, at) (correlator.ChannelDucting, bool)`. It is built on the existing `fresh()` and `byChannel`, and `MNegative` and `LRFMNegative` stay as they are.
- **[beater/baseline.go](tropobeat/beater/baseline.go).** Add a small adapter, `correlatorBaselines{*baselineStore}`, whose `Lookup` maps `Baseline` to `correlator.Baseline`.
- **[beater/tropobeat.go](tropobeat/beater/tropobeat.go).**
  - `New()`: when `rabbit != nil`, create `bt.correlator` with both sources (and `RealClock`).
  - `consume()`: call `bt.correlator.Submit(r)` before `Ack`; if it returns false (the engine is closed, so we're shutting down), `Nack` with requeue.
  - `consume()`: add `case ev := <-events:` to publish `correlationEvent(ev, snap)`. `events` is a nil channel when the correlator is off.
  - `consume()`: add `case tr := <-traces:` to publish `correlationTraceEvent(tr, snap)`. `traces` is a nil channel when no category is enabled.
  - `New()`: install the trace observer with the configured category mask and LRF filter.
  - `consume()`: extend the minute summary with the correlator stats (instances live, pairs by state, no-baseline count, traces published and dropped).
  - `Stop()`: close the correlator after the RabbitMQ client.
- **[beater/events.go](tropobeat/beater/events.go).**
  - Add `metricsetCorrelation = "correlation"` and `correlationEvent(ev, snap)`, which writes fields under `tropobeat.correlation.*` (state, kind, episode, sides, ducting evidence, signature).
  - Fill the shared identifying fields with the existing `keys` helper (`addPath` for every path on the channel, plus the LRF, channel, band and own stations), so the Ducting Watch filters apply to correlation events too.
  - **LRF location.** Every correlation event carries `tropobeat.correlation.lrf.location` (`geo_point`, `{lat, lon}`), using the same shape and lookup as `tropobeat.baseline.lrf.location`. The point comes from the current snapshot's LRF list (`snap.LRFs`, keyed by `demod.NormalizeLRF(code)`). It is left out only when the LRF code isn't in the LRF list (Q14). Add the field to `fields.yml` as `geo_point`.
  - Fill in the `baseline` event's deferred `ber.spike_threshold` and `mer.drop_threshold_db` from the correlator config.
- **[config/config.go](tropobeat/config/config.go).** A new `Correlator` section (`correlator:`) holding the fields above plus `publish: true`, with defaults in `DefaultConfig` and validation (positive durations, `ber_factor` > 1, `hysteresis_cycles` ≥ 1).
- **Fields and config docs.**
  - [_meta/fields.yml](tropobeat/_meta/fields.yml) gets a `tropobeat.correlation` group with strict types: BER is `double`, times are `date`, and states are `keyword`.
  - `_meta/config/*.tmpl` get the `correlator:` section.
  - Regenerate with mage. **Then restore `tropobeat.yml` from git and splice in only the new section**, because regenerating overwrites the user's test settings. Check that `git diff tropobeat.yml` shows additions only.

## 3. Test data: every condition reproducible with mock weather

Goal: one replay run, together with the mock weather server, shows every per-input condition, every hysteresis transition, every pair state, both signatures, and every trace reason, each in a known place and at a known time.

### 3a. Mock weather additions ([internal/openmeteo/mock](tropobeat/internal/openmeteo/mock/mock.go))

The mock currently serves one global scenario, or a mixed cycle that varies with place, so a replay can't be lined up with it. Add:

- **`openmeteo.mock.sites`.** A per-LRF scenario override, for example `{TERH: trapped, PROV: ducting, BOS: normal}`. It applies to every point of that LRF's co-channel paths (the LRF, midpoint and transmitter). Every other LRF keeps the global scenario.
  - The handler only sees coordinates, so tropobeat hands the mock a coordinate → scenario map when it starts it.
  - That map is built from the snapshot's paths and refreshed after each discovery run.
  - Validation rejects unknown scenario names.
- **`openmeteo.mock.fail_every` / `fail_for`.** These make the mock return HTTP 503 for `fail_for`, once every `fail_every` (0 = never). Polls then fail and the ducting results go stale after 2 × period, which is how `dual_ducting_unknown` (`poll_stale`) is reached in the middle of a run. Because it repeats, it keeps working during a continuous demo. `no_poll` is also reached at start-up, before the first poll.

### 3b. Replay scenario file ([tools/demodreplay](tropobeat/tools/demodreplay/main.go))

**New flag: `-scenario file.yml`.** The file holds a list of timed injections, with offsets measured from the start of the replay. Each step names an LRF, a channel (or `*` for every channel on a frame), a side (`A`, `B`, `AB`), a start offset, a duration and an action:

| Action | Rewrites the matching reports | Exercises |
|---|---|---|
| `ber` | BER set to N × the instance's own median (default 50×), with MER and power unchanged | `ber_spike`, Environmentally Stressed, power `steady_or_rising` |
| `saturate` | BER set to 16.777215 | `ber_saturated` |
| `mer` | MER lowered by N dB (default 6), power unchanged | `mer_drop` |
| `fade` | MER lowered and power lowered by N dB | power signature `dropping` |
| `unlock` | Status `Not Locked`, MER 0, power −80, as real unlocked reports look | `lock_lost`; with channel `*`, the whole frame goes down, so the guard gives `guard_frame_down` |
| `drop` | Reports withheld | `unpaired` / `other_side_unknown`, `other_side_stale`, `gap_reset`, `expired` |
| `noside` | Side label blanked | `no_side` skip |
| `flap` | Alternates bad and clean every N reports | hysteresis never entering (e.g. 2 bad / 2 clean) or entering then clearing |

- **`-quiet`.** Rewrites the replayed sample's own readings for the scenario's LRFs to healthy values: BER 0, and MER at that instance's median, computed from the loaded sample. Only injected conditions then appear on those sites, while all other sites replay naturally.
- Each injection logs a line when it starts and ends, giving a timeline to check the published events against.

**Continuous mode for demos.** Today the tool plays the 15-minute sample (14:57–15:11) once, then exits, and any publish error is fatal.
- **`-loop`.** Replays the sample endlessly. Each pass's timestamps continue from the previous pass: each pass is shifted by the sample span plus one report interval. Every instance therefore keeps its once-a-minute cadence across the seam, with no gaps, duplicates or jumps backwards that would trip the hysteresis or the gap rules. Requires `-now`.
- **Scenario repeat.** The scenario file has `repeat_every` (default: the end of its last step plus 5 minutes; 0 = run once). Its timeline is independent of the sample loop, so a 40-minute scenario repeats on its own cycle over the 15-minute sample passes.
- **Reconnects.** A dropped connection or channel no longer stops the tool. It reconnects with backoff (1s up to 30s, the same as tropobeat) and resumes at the current time position, without a burst of overdue reports. Reports due while disconnected are skipped and counted.
- **Status.** A periodic status line every minute: current pass, reports published and skipped, active injections, time to the next scenario step. A clean Ctrl-C or SIGTERM ends it.
- **Demo setup.**
  - A second shipped scenario, `scenarios/demo.yml`: a 60-minute cycle of fewer, longer, presentation-paced episodes, run with `-loop` and `repeat_every`. It covers a 15-minute trapping CCI with ongoing events, a ducting CCI, a `dual_unexplained` fade, a `single_side`, and a frame down.
  - README gives an example systemd unit (or `nohup`) for leaving the replay and tropobeat running unattended.

### 3c. Shipped scenario: `tools/demodreplay/scenarios/all-conditions.yml` (plus a matching `tropobeat.yml` snippet)

- The snippet uses `lrf_sites` with about 4 LRFs, `period: 1m`, `mock.sites` and a `fail_after` window.
- The scenario walks through every state, one LRF or channel each, in about 40 minutes at real time (`-speed` shortens it, as long as `period` is scaled to match):
  1. **CCI with trapping.** AB `ber` on a TERH channel; mock `trapped` → `cci_confirmed`, strength `trapping`, held for over 5 minutes so an `ongoing` event appears, then a clear with its duration.
  2. **CCI with ducting.** AB `mer` on a PROV channel; mock `ducting` → `cci_confirmed`, strength `ducting`.
  3. **No ducting.** AB `fade` on BOS; mock `normal` → `dual_unexplained`, signature `dropping`.
  4. **Ducting unknown.** AB `ber` during the mock fail window → `dual_ducting_unknown`.
  5. **One side only.** A `ber` → `single_side`.
  6. **Other side missing.** A `ber` with B `drop` → `unpaired` / `other_side_stale`.
  7. **Lock loss on one channel.** AB `unlock` on one channel at a trapped LRF → `cci_confirmed` through lock loss.
  8. **Whole frame down.** A `unlock` on channel `*` → `guard_frame_down`; no CCI.
  9. **Saturated BER.** AB `saturate` → `ber_saturated`.
  10. **Flapping.** `flap` 2/2 → never enters; `flap` 4/4 → enters, then clears.
  11. **Gaps.** `drop` for 1 minute (no counter change), then for 4 minutes (`gap_reset` / `expired`).
  12. **Missing side label.** `noside` → `no_side` skip.
- The scenario file documents which channel each step uses. Steps 1, 2 and 7 need channels that have co-channel paths, and the snippet's comments give the reason.
- README and CORRELATION.md say how to run it.

### Tests

- Replay: scenario parsing, and each action's rewrite applied to fixture reports.
- Replay loop seam: a fixture instance's timestamps stay about one minute apart and strictly increasing across passes.
- Scenario `repeat_every` scheduling.
- Reconnect skips overdue reports rather than bursting them (using a fake publisher).
- Mock: a per-site override serves the chosen scenario's profile at that LRF's points, and the fail window returns 503.

## 4. Docs

### CORRELATION.md: correlator specification (new; sits beside DUCTING.md, linked from the README)

**Audience.** Broadcast operations staff and RF engineers. It explains how tropobeat decides that co-channel interference (CCI) is the root cause, at a high level.

**What it leaves out.** Nothing from inside the program: no function, type, package or file names, no data structures, and no goroutines or channels. Setting names and published field names are the only exception. They appear in the settings and field reference sections only, because operators use them. The style follows DUCTING.md and the customer-facing rolling plan.

**Outline:**
1. **What the correlator answers.** "Is this channel's degradation caused by a distant co-channel station arriving over a tropospheric duct?", and what it does *not* decide (outages and frame faults, which are planned).
2. **Evidence it combines.** Each demod input's live readings (MER, Pre-FEC BER, input power, lock) once a minute; the input's 7-day baseline; the co-channel paths found for the LRF's channel; and the latest ducting state along each path. A simple diagram shows the evidence flowing into one verdict.
3. **Step 1: is one input stressed?**
   - BER spike against the effective baseline (with the 1e-6 floor and why it exists), saturated readings, MER drop, and lock loss.
   - A worked example with numbers (e.g. TERH side A, RF 10: baseline MER 33 dB, so 30 dB or below is a drop).
4. **Step 2: is it sustained?** The 3-minute rule to start and to clear, why it exists (to avoid flapping), and how missing minutes and long gaps are treated. A small timeline illustration.
5. **Step 3: do sides A and B agree?**
   - The shared antenna feed, and why agreement rules out a single demod but is not, on its own, proof of interference.
   - What "fresh" means: reports within the last 90 seconds.
   - Channels received twice on one frame, and inputs with no side label.
   - The lock-loss guard: why a whole frame or site going dark is not treated as CCI.
6. **Step 4: is there a ducting path on this channel?**
   - dM/dh < 0 on any co-channel path, with trapping as the stronger evidence.
   - How the interfering transmitter is named: the strongest path, with ties going to the nearest transmitter.
   - How old ducting data may be: two weather polls.
   - Channels with no co-channel paths.
7. **The verdicts.**
   - A table of every pair state (`clear`, `single_side`, `unpaired`, `dual_ducting_unknown`, `dual_unexplained`, `cci_confirmed`): its meaning, and what an operator should check for each.
   - The likely non-ducting causes behind `dual_unexplained`.
8. **Supporting evidence: the power signature.** Power steady or rising points to interference; power falling points to a fade, the feed or the transmitter. It never decides a verdict on its own.
9. **Events over an episode.** Start, update, ongoing (every 5 minutes), and clear with duration and peak state; how to read one episode end to end.
10. **Tracing a decision.** What each trace category shows, the reason codes in plain language, which categories are on by default, and how to use the Correlation debug dashboard to answer "why didn't this alert?" and "why did it?".
11. **Settings reference.** Thresholds, timings and trace settings, with defaults and the effect of changing each.
12. **Event field reference.** The `correlation` and `correlation_trace` fields.
13. **Limits and open points.**
    - Thresholds are not yet calibrated (Q26), and the ducting resolution is limited (Q17).
    - Transmitters outside the FCC set are not considered (Q11).
    - Outage and frame-fault rules are planned.
    - Sites whose LRF codes don't match are skipped (Q14).

**Linking.**
- The README gets a short correlation section that points to CORRELATION.md for the detail, the same way it points to DUCTING.md.
- The rolling plan's Phase 4b section and architecture notes link to it.
- DUCTING.md links to it from its "how results are used" text.

**Kept in step.** CORRELATION.md is updated whenever a rule, a threshold default or a verdict changes. This is added to the rolling plan's maintenance note.

### Other doc updates

- **ROLLING_PLAN.md** (customer-facing style):
  - Mark the 4b rows: BER/MER and lock loss, hysteresis, dual-path confirmation with ducting, signature. Outage exclusion and frame loss are re-marked as deferred.
  - Add decisions: lock loss counts per channel; the ducting gate is dM/dh < 0 with trapping ranked; pair states; the frame guard; the `correlation` metricset.
  - Narrow 4c to snapshots, alerts and inSITE. Add a change-log line.
- **README.md.** Add a correlation section: settings, pair states, event fields, and the trace categories with how to enable them.
- **DASHBOARD.md.** Add a Correlation debug section.

## Verification

1. Run `go test ./...` in `tropobeat/`. New `internal/correlator` tests use a fake clock and fake sources:
   - Hysteresis enters after 3 reports and clears after 3; a gap counts neither way; a long gap resets.
   - A+B with ducting gives `cci_confirmed`, and with trapping gives `trapping` strength.
   - A only gives `single_side`; A+B with no ducting gives `dual_unexplained`; stale ducting gives `dual_ducting_unknown`.
   - A one-sided channel gives `unpaired`.
   - Lock loss with the frame's other inputs locked confirms; a whole-frame unlock doesn't.
   - A twice-received channel counts its side as degraded when either input is.
   - Update events fire when evidence changes, and clear events carry the duration.
   - An `ongoing` event follows every 5 minutes with the same episode ID while a pair is not clear; none follows once it clears or when `snapshot_interval` is 0.
   - Beater tests cover the `correlationEvent` shape, `pathDuctingFrom` state and transmitter fields, and the config defaults.
2. **All-conditions run.** Use a local RabbitMQ, the shipped snippet and `demodreplay -now -quiet -scenario scenarios/all-conditions.yml`. Then check, against the injection timeline in the replay log:
   - every pair state appears in the `correlation` metricset at the expected LRF, channel and time;
   - every reason code in the 1b table appears in `correlation_trace` (with `input`, `skip`, `ducting` and `sweep` also enabled);
   - the Correlation debug dashboard shows each step.

   **Demo soak.** Run `demodreplay -now -loop -quiet -scenario scenarios/demo.yml` with tropobeat for at least 2 hours. Check:
   - each demo episode recurs every 60 minutes;
   - no `gap_reset` or `expired` reasons appear at the sample seams;
   - restarting the RabbitMQ broker mid-run is survived by both tropobeat and the replay.

   Ad-hoc check: a local RabbitMQ with `openmeteo.mock` mixed mode and `lrf_sites` set to a few LRFs (e.g. TERH, PROV, BOS), using `period: 1m` from the test `tropobeat.yml`. Replay the sample with `--degrade` for each mode on a channel that has co-channel paths. Check the log summary for pair states, and check the tropobeat index for `metricset.name: correlation` events: `cci_confirmed` naming the strongest path's transmitter, then `clear` with a duration. Each event's `lrf.location` should be mapped as `geo_point` and should match the LRF's baseline event.
3. Trace checks:
   - With the defaults, every `correlation` event has matching `pair` and `hysteresis` trace records with the same `episode_id`.
   - Enable `input` with `trace.lrf_sites: [TERH]`, and confirm that only TERH input records appear and their thresholds match TERH's baseline event.
   - A `--degrade` run on side A only yields `pair` records with reason `single_side`; stopping side B's frame in the replay yields `other_side_stale`.
   - Shrink `trace.buffer` and confirm drops are counted in the summary log, and that acks and `correlation` events are unaffected.
   - Unit tests confirm that a disabled category builds no record (the observer is never called).
   - Load the Correlation debug dashboard into a clean Kibana space with the dashboard setup.
4. Replay without `--degrade` over the full sample, and count the episodes by state as a reasonableness check against the 4a simulation (805 A+B episodes a day before the ducting gate). This checks the mechanics only, not calibration (Q26).
