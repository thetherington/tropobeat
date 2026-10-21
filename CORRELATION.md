# Correlation in tropobeat

How tropobeat decides, minute by minute, whether a channel's degradation at an LRF is co-channel interference (CCI) carried by a tropospheric duct, what each verdict means, and how to trace a decision.

For how ducting is calculated, see [DUCTING.md](DUCTING.md). For project status and open questions, see the [rolling plan](ROLLING_PLAN.md).

## Contents

1. [What the correlator answers](#1-what-the-correlator-answers)
2. [The evidence it combines](#2-the-evidence-it-combines)
3. [Step 1: is one input stressed?](#3-step-1-is-one-input-stressed)
4. [Step 2: is it sustained?](#4-step-2-is-it-sustained)
5. [Step 3: do sides A and B agree?](#5-step-3-do-sides-a-and-b-agree)
6. [Step 4: is a co-channel path ducting?](#6-step-4-is-a-co-channel-path-ducting)
7. [The verdicts](#7-the-verdicts)
8. [Supporting evidence: the power signature](#8-supporting-evidence-the-power-signature)
9. [Events over an episode](#9-events-over-an-episode)
10. [Tracing a decision](#10-tracing-a-decision)
11. [Settings reference](#11-settings-reference)
12. [Field reference](#12-field-reference)
13. [Testing and demonstrations](#13-testing-and-demonstrations)
14. [Limits and open points](#14-limits-and-open-points)

---

## 1. What the correlator answers

For every LRF and RF channel, the correlator answers one question:

> Is this channel's degradation caused by a distant station on the same channel, arriving over a tropospheric duct?

It answers as the demod reports arrive, about once a minute per demod input, and names the most likely interfering transmitter when the answer is yes.

What it does **not** decide yet: whether a whole LRF is down for a power or backhaul fault (an outage), or whether a demod frame has failed. Those rules are planned. Until then, the correlator makes sure a dark frame or site is never mistaken for interference (see [the lock-loss guard](#the-lock-loss-guard)).

## 2. The evidence it combines

```
 Live demod reports            7-day baselines            Co-channel paths + ducting
 (MER, Pre-FEC BER,            (median BER and MER        (every same-channel transmitter
  power, lock; ~1/min)          of each input)             within 200 miles, and the
          │                          │                     latest ducting along each path)
          └──────────┬───────────────┘                                  │
                     ▼                                                  │
        Step 1  Is this input stressed?                                 │
        Step 2  Has it been for 3 reports in a row?                     │
                     │                                                  │
                     ▼                                                  │
        Step 3  Are side A and side B both degraded?                    │
                     │                                                  │
                     ▼                                                  ▼
        Step 4  Is a co-channel path on this channel ducting? ◄─────────┘
                     │
                     ▼
              One verdict per LRF and channel
```

| Evidence | Where it comes from | How fresh |
|---|---|---|
| Live readings | Each demod input's report: MER, Pre-FEC BER, input power and lock status | About once a minute |
| Baseline | The median BER and MER of the input's locked reports over the last 7 whole days | Recomputed daily |
| Co-channel paths | Daily co-channel discovery: every other station on the channel within 200 miles of the LRF | Daily |
| Ducting | The ducting state of each path (normal, super-refractive, ducting, trapping) from the latest weather poll | Every weather poll |

## 3. Step 1: is one input stressed?

Every report is compared with **that input's own baseline**, not with a network-wide figure, because what is normal differs from channel to channel and LRF to LRF. A report can show four anomalies:

| Anomaly | Rule | Default |
|---|---|---|
| BER spike | The input is locked and its Pre-FEC BER is at least 10 × its effective baseline BER | `ber_factor: 10` |
| Saturated BER | The input is locked and reports the saturated BER value (16.777215), which means the demod can't measure it | — |
| MER drop | The input is locked and its MER is at least 3 dB below its baseline median | `mer_drop_db: 3` |
| Lock loss | The input is not locked, although it is normally locked (it has a usable baseline) | `lock_loss: true` |

**Effective baseline BER.** Most inputs report a BER of exactly 0 almost all the time, so their median is 0, and any non-zero reading would be an "infinite" spike. The baseline therefore never goes below 1e-6 (the `baseline.ber_floor` setting). For a typical input the spike threshold is 10 × 1e-6 = 1e-5.

**Worked example (TERH, side A, RF 31).** The baseline median MER is 20 dB, so 17 dB or below is a MER drop. The median BER is 2e-6, above the floor, so the effective BER is 2e-6 and the spike threshold is 2e-5.

| Report | MER | BER | Anomalies |
|---|---|---|---|
| Normal | 20 dB | 0 | none |
| Interference starts | 20 dB | 2.5e-2 | BER spike (2.5e-2 ≥ 2e-5) |
| Worse | 16 dB | 3e-2 | BER spike, MER drop (16 ≤ 20 − 3) |
| Signal lost | — | — | lock loss |

Inputs without a usable baseline are not judged. That covers spare and test inputs, inputs with less than a day of reports, inputs whose median BER is saturated, and every input while the baselines are more than two days old. These inputs are visible in the decision trace (category `skip`).

## 4. Step 2: is it sustained?

One bad report is not enough. An input becomes **degraded** after **3 anomalous reports in a row**, and stops being degraded after **3 clean reports in a row** (`hysteresis_cycles: 3`). With one report a minute, a condition must last about 3 minutes to start, and the channel must be clean for about 3 minutes to clear. This stops a marginal channel that dips for a minute from raising and clearing alerts all day.

```
report:     1    2    3    4    5    6    7    8    9   10
anomaly:    ✗    ✗    ✗    ✗    ✓    ✗    ✓    ✓    ✓    ✓
bad count:  1    2    3    4    0    1    0    0    0    0
good count: 0    0    0    0    1    0    1    2    3    4
degraded:   -    -    YES  YES  YES  YES  YES  YES  -    -
                     ▲ enters                       ▲ clears
```

- **A missing minute counts neither way.** If one report doesn't arrive, the counts carry on from where they were.
- **A long gap starts again.** If an input sends nothing for more than 3 minutes (`expire_after`), its live state is dropped, and when it returns its counts start from zero.
- **Environmentally Stressed.** Separately, an input whose BER spike lasts 3 reports in a row while it stays locked is flagged *Stressed*, the original rule for environmental stress. It is shown with the input's evidence.

## 5. Step 3: do sides A and B agree?

Each LRF receives every channel on two demods, side A and side B, fed from **one shared antenna**. So:

- If **only one side** is degraded, the received signal is fine and the problem is in that demod or its card. That is not interference.
- If **both sides** are degraded, the problem is in the received signal itself. Interference is one possible cause, but so are a fault at the station's own transmitter, the shared antenna or preamp, or a fade. A+B agreement alone is **not** proof of interference; Step 4 supplies the environmental evidence.

**Fresh reports only.** A side counts only if one of its inputs has reported within the last 90 seconds (`stale_after`). A side whose inputs have gone quiet can't be judged.

**Channels received twice.** A few channels are received on two inputs of the same frame. The side counts as degraded if either of its inputs is.

**Missing side labels.** An input that reports no side (or an unrecognised one) is still judged, but is left out of A/B pairing (rolling plan Q23).

**Recovering inputs.** An input stays degraded until it has 3 clean reports in a row. While its reports are already clean, its side counts as *recovering*: that keeps a verdict that side was already part of going until the input clears, but never starts a new one, never joins one it wasn't part of, and never changes the verdict to a different one: when only recovering sides are left, the verdict stays as it is until it clears. A frame coming back after an outage, or the last minutes of an episode, therefore raise nothing new, and a side whose frame was down can't turn the other side's verdict into a two-sided one as it comes back.

**Waiting for the other side.** When interference starts, the two sides rarely cross the 3-report mark in the same minute. So that a real A+B event doesn't first appear as "one side only", a one-sided verdict must hold for 2 minutes (`pair_grace`) before it is reported. The same applies at the end of an episode, when one side clears a minute before the other.

### The lock-loss guard

A channel that loses lock counts as degraded only if **another input on the same demod frame is still locked**. That keeps two different situations apart:

- One channel unlocks while the rest of the frame is fine: the signal on that channel has collapsed. This is a strong CCI symptom, and it counts.
- Every input on a frame unlocks at once: the frame, its power or its network link has failed. It is not treated as degraded signal, so it can never confirm CCI.

## 6. Step 4: is a co-channel path ducting?

When both sides are degraded, the correlator looks up every co-channel path on that LRF and channel in the latest weather poll.

- **The environmental evidence** is a path where modified refractivity falls with height (dM/dh < 0) at any of its points. That is a path whose ducting state is `ducting` or `trapping` ([DUCTING.md](DUCTING.md#ducting-state)).
- **Strength.** `trapping` (a duct thick enough to trap the channel's frequency) is stronger evidence than `ducting` (a ducting layer that may be too thin for the channel). The verdict records which.
- **The likely interferer** is the transmitter of the path with the highest ducting state. If several paths are equally strong, the nearest transmitter is named. Every path that is ducting or trapping is listed too.
- **How old ducting may be.** Ducting results older than two weather polls (2 × `period`) are not used: stale ducting can never confirm interference.
- **Paths with no weather.** If some of a channel's paths had no usable weather in the latest poll and none of the others is ducting, the ducting is treated as unknown, not as absent.
- **Channels with no co-channel paths.** There is no transmitter within 200 miles to interfere, so ducting can't explain the degradation.

## 7. The verdicts

Each LRF and channel has one verdict at a time.

| Verdict | Meaning | What to check |
|---|---|---|
| `clear` | Neither side is degraded. | Nothing. |
| `single_side` | One side is degraded; the other is clean. | The degraded demod, its card and its configuration. Not interference. |
| `unpaired` | One side is degraded, and the other can't be judged: there is no input on that side, it has stopped reporting, or its whole frame is down. | Why the other side isn't reporting, or whether its frame is up. CCI can't be confirmed. |
| `dual_ducting_unknown` | Both sides are degraded, but there are no recent ducting results (no weather poll yet, polls failing, or no usable weather on the paths). | The weather polling. Treat as a received-signal problem of unknown cause until ducting is available. |
| `dual_unexplained` | Both sides are degraded, the ducting is known, and no co-channel path on the channel is ducting (or the channel has none). | The causes ducting doesn't explain; see below. |
| `cci_confirmed` | Both sides are degraded **and** a co-channel path on the channel is ducting or trapping. | The named transmitter. Expect the degradation to follow the duct and clear when the duct breaks up. |

The verdicts are ordered by severity, from `clear` (0) to `cci_confirmed` (5), so dashboards can show the worst verdict over a time range.

**Behind `dual_unexplained`.** The signal really is degraded, but ducting doesn't account for it. Likely causes:

- The local station's own transmitter (a fault, reduced power or maintenance).
- The shared antenna, preamp, splitter or cabling: A and B share it, so both degrade together.
- Interference ducting can't explain: adjacent-channel signals, impulse noise or mobile networks.
- An interferer tropobeat doesn't consider: one more than 200 miles away, or a low-power, Class A, translator or distributed transmission station (rolling plan Q11).
- A thin duct the weather model's levels can't resolve (rolling plan Q17).

The power signature below helps tell these apart.

## 8. Supporting evidence: the power signature

When an input becomes degraded, its power is compared with its typical power just before. That is the median of its last clean locked readings.

| Signature | Power | Typical of |
|---|---|---|
| `steady_or_rising` | Held, or risen | Interference: a second signal is *adding* energy on the channel. |
| `dropping` | Fallen by 3 dB or more (`power_drop_db`) | A fade, or a fault in the feed or the station's transmitter: the wanted signal is weaker. |
| `unlocked` | — | The input has lost lock. |

The signature is supporting evidence only. It is recorded with every verdict, but it never decides one.

## 9. Events over an episode

An **episode** is one period during which an LRF and channel is not `clear`. It has one ID, shared by every event and trace record about it.

| Event | When |
|---|---|
| `start` | The verdict leaves `clear`. |
| `update` | The verdict changes within the episode (for example from `single_side` to `cci_confirmed`), or, while `cci_confirmed`, the evidence changes: the strength moves between ducting and trapping, or a different path becomes the strongest. |
| `ongoing` | Every 5 minutes (`snapshot_interval`) while the episode lasts, with the current evidence and the time so far, so a dashboard looking at a short time range still sees an active episode. |
| `clear` | The verdict returns to `clear`. The event carries the episode's duration and its most severe verdict. |

A recorded CCI episode on TERH RF 31 (both sides' BER near 2.5e-2 against a 2e-5 threshold, with trapping on all 8 co-channel paths):

```
18:32:30  start    cci_confirmed (trapping)   both sides degraded; likely interferer WKMA-TV, 157 mi
18:37:36  ongoing  cci_confirmed (trapping)   5 min so far
18:42:46  ongoing  cci_confirmed (trapping)   10 min so far
18:44:26  clear                               lasted 11 min 56 s, peak cci_confirmed
```

If the duct had thinned during the episode, so that the strongest path went from trapping to ducting, or a different transmitter became the strongest, an `update` event would have appeared between the start and the clear.

Every event also carries, for each side, the inputs, their latest BER, MER and power, the baseline values and thresholds they were judged against, their anomalies and the power signature. It also carries the ducting evidence: the poll time, the forecast hour, the strongest path and every ducting path.

## 10. Tracing a decision

Alongside its verdicts, the correlator can publish a **decision trace**: a record of each judgement it makes, with a reason code and a one-line explanation. It answers "why did this alert?" and, just as importantly, "why didn't it?". Trace records are published in categories that can be turned on separately (`trace.categories`).

| Category | Records | On by default | Volume |
|---|---|---|---|
| `pair` | A/B verdict decisions, when a verdict is not clear or changes (at most once a minute while unchanged) | yes | low |
| `hysteresis` | Inputs becoming or ceasing to be degraded or Stressed, gaps, expiries | yes | low |
| `ducting` | The path-by-path ducting looked up for a channel with a degraded side | no | low |
| `skip` | Reports that couldn't be judged, at most once per input per hour | no | low |
| `input` | Every report judged: values, thresholds and counts | no | about one per input a minute; use with `trace.lrf_sites` |
| `sweep` | A summary of the correlator's periodic check | no | 6 a minute |

Each record has a **decision**: `hit` (an anomaly or CCI was found), `miss` (it wasn't) or `skip` (not judged, or a verdict waiting for the other side).

### Reason codes

| Category | Reason | Meaning |
|---|---|---|
| input | `ok` | Within its baseline. |
| input | `ber_spike`, `ber_saturated`, `mer_drop`, `lock_lost` | The anomalies of [Step 1](#3-step-1-is-one-input-stressed). |
| skip | `no_baseline` | No baseline for this input and channel. |
| skip | `baseline_insufficient`, `baseline_invalid`, `baseline_no_station` | The baseline can't be used (too few reports, saturated median, or a spare input). |
| skip | `baseline_stale` | The baselines are older than `baseline.max_age`; no input is judged until a baseline run succeeds. |
| skip | `no_side` | Judged, but left out of A/B pairing (no side label). |
| hysteresis | `degraded_enter`, `degraded_clear` | 3 anomalous (or clean) reports in a row. |
| hysteresis | `stressed_enter`, `stressed_clear` | Environmentally Stressed starts or ends. |
| hysteresis | `expired` | No report for over `expire_after`: the input's live state is dropped. |
| hysteresis | `gap_reset` | The input reported again after such a gap; its counts start from zero. |
| pair | `clear` | Neither side degraded. |
| pair | `single_side` | One side degraded, the other clean. |
| pair | `other_side_stale` | The other side has no report within `stale_after`. |
| pair | `other_side_missing` | No input on the other side is known for this channel. |
| pair | `guard_frame_down` | A side is degraded only by lock loss while its whole frame is unlocked. |
| pair | `ducting_no_poll`, `ducting_stale`, `ducting_incomplete` | Both sides degraded, ducting unknown: no poll yet, the last poll is too old, or paths without usable weather. |
| pair | `no_cochannel_paths` | Both sides degraded; the channel has no co-channel paths. |
| pair | `ducting_none_on_channel` | Both sides degraded; no co-channel path on the channel is ducting. |
| pair | `cci_confirmed` | Both sides degraded and a path is ducting or trapping. |
| ducting | `path_trapping`, `path_ducting`, `path_below_ducting` | The strongest path's ducting state. |
| ducting | `path_no_data`, `no_poll`, `poll_stale`, `no_cochannel_paths` | Why ducting is unknown or absent. |
| sweep | `summary` | Inputs live, expired, pairs, pairs not clear, trace records dropped. |

A one-sided verdict that is waiting for `pair_grace` has decision `skip`, and its message says how long it has waited.

### Using the trace

- **Why didn't this channel alert?** Filter the trace on the LRF and channel. A `pair` record with `other_side_stale` means side B wasn't reporting. `ducting_none_on_channel` means no path was ducting. If there are no `hysteresis` records at all, neither input stayed anomalous for 3 reports; turn on `input` for that LRF to see each report against its thresholds.
- **Why did it alert?** The `cci_confirmed` pair record and its `ducting` record show both sides' readings and the path that confirmed it.
- **Is the correlator seeing everything?** `skip` shows the inputs it can't judge, and `sweep` shows how many inputs are live.

The **Correlation debug** dashboard brings these together ([DASHBOARD.md](DASHBOARD.md#4-correlation-debug)). Trace records can also be written to the tropobeat log (`trace.log: true`).

If tropobeat produces trace records faster than they can be published, the excess is dropped and counted (`trace.buffer`). Verdicts are never dropped.

## 11. Settings reference

All settings are in the `correlator:` section of `tropobeat.yml`. The correlator runs only when the live demod stream (`rabbitmq.enabled`) is on.

| Setting | Default | Effect |
|---|---|---|
| `enabled` | `true` | Run the correlator on the live demod stream. |
| `publish` | `true` | Publish verdicts as the `correlation` metricset. |
| `ber_factor` | `10` | BER spike: at least this many times the effective baseline BER. Higher means fewer, stronger spikes. |
| `mer_drop_db` | `3` | MER drop: at least this many dB below the baseline median. |
| `lock_loss` | `true` | Count loss of lock on a normally locked input as an anomaly. |
| `hysteresis_cycles` | `3` | Reports in a row needed to become degraded, and to clear. |
| `power_drop_db` | `3` | Power fall that makes the signature `dropping`. |
| `stale_after` | `90s` | How old an input's last report can be and still count for its side. |
| `expire_after` | `3m` | An input silent this long loses its live state; a gap this long restarts its counts. |
| `pair_grace` | `2m` | How long a one-sided verdict must hold before it is reported. |
| `snapshot_interval` | `5m` | How often an episode is re-published (`ongoing`). `0` turns it off. |
| `sweep_interval` | `10s` | How often silent inputs, waiting verdicts, ducting changes and snapshots are checked. |
| `input_buffer` | `1024` | Reports that can wait for the correlator. |
| `trace.categories` | `[pair, hysteresis]` | Trace categories to publish. `[]` turns tracing off. A list replaces the default list. |
| `trace.lrf_sites` | `[]` (all) | Limit tracing (except `sweep`) to these LRF codes. |
| `trace.buffer` | `4096` | Trace records that can wait to be published before new ones are dropped. |
| `trace.log` | `false` | Also write trace records to the log at debug level (selector `correlator`). |

The thresholds are initial values. They need tuning against real multi-day telemetry (rolling plan Q26).

## 12. Field reference

### `correlation` events

Fields under `tropobeat.correlation`. Each event also carries the shared fields (`tropobeat.lrf_codes`, `rf_channels`, `bands`, `path_ids`, `transmitter_call_signs`, `station_call_signs`, `discovery_run_id`, and `ducting_state` of the strongest path), so every Ducting Watch filter applies.

| Field | Meaning |
|---|---|
| `kind` | `start`, `update`, `ongoing` or `clear` |
| `state`, `previous_state` | The verdict, and the one before (not on `ongoing`) |
| `severity` | The verdict as 0 (`clear`) to 5 (`cci_confirmed`) |
| `reason` | The rule that decided it (the pair reason codes above) |
| `strength` | For `cci_confirmed`: `ducting` or `trapping` |
| `episode.id`, `episode.start`, `episode.duration_s`, `episode.peak_state` | The episode |
| `lrf.location` | The LRF's location (geo point), for maps |
| `sides.a`, `sides.b` | Each side: `view` (`degraded`, `recovering`, `clean`, `guarded`, `stale`, `missing`), `since`, the side's anomalies (`conditions`), worst `ber`, `mer_db` and `power_dbm`, `signatures`, `instance_ids`, and every input under `instances` |
| `sides.*.instances` | Per input: `id`, `host_ip`, `slot`, `input`, `side`, `card_label`, `frame_label`, `report_time`, `fresh`, `locked`, `ber`, `mer_db`, `power_dbm`, `ber_effective`, `ber_threshold`, `mer_median_db`, `mer_threshold_db`, `conditions`, `degraded`, `stressed`, `bad`, `good`, `signature`, `power_delta_db` |
| `ducting.status` | `fresh`, `stale` or `no_poll` |
| `ducting.polled_at`, `ducting.forecast_time` | When the ducting was computed, and the hour it applies to |
| `ducting.paths_on_channel`, `ducting.paths_unknown` | Co-channel paths on the channel, and how many had no usable weather |
| `ducting.strongest` | The strongest path: `path_id`, `call_sign`, `facility_id`, `distance_mi`, `state`, `level`, `ducting_points` (which of LRF, midpoint, transmitter has dM/dh < 0) |
| `ducting.active_path_ids`, `ducting.active_call_signs` | Every path that is ducting or trapping, and its transmitter |

### `correlation_trace` records

Fields under `tropobeat.correlation_trace`, with the same shared fields and `lrf.location`.

| Field | Meaning |
|---|---|
| `category`, `reason`, `decision`, `message` | What was decided, and why (see [Tracing a decision](#10-tracing-a-decision)) |
| `episode_id` | The episode the record belongs to, if any |
| `state` | For `pair` records: the verdict after the decision |
| `input` | For `input`, `skip` and `hysteresis` records: the input and its reading (the same fields as `sides.*.instances`) |
| `sides`, `ducting` | For `pair` and `ducting` records: as on `correlation` events |
| `sweep` | For `sweep` records: `instances`, `expired`, `pairs`, `not_clear`, `reevaluated`, `trace_dropped` |

The `baseline` metricset also carries the thresholds each input is judged against: `ber.spike_threshold` and `mer.drop_threshold_db`.

## 13. Testing and demonstrations

Tropobeat ships a replay tool that publishes the demod telemetry sample to RabbitMQ. It can also inject conditions on chosen LRFs, channels and sides at set times. With mock weather set per LRF, every verdict can be reproduced on purpose, without inSITE or a real ducting event.

| What | File |
|---|---|
| Every condition, one LRF and channel each, in about 45 minutes | `tools/demodreplay/scenarios/all-conditions.yml` |
| A 60-minute demonstration cycle for unattended demos | `tools/demodreplay/scenarios/demo.yml` |
| The matching tropobeat settings (LRFs, 1-minute polls, mock weather per LRF, tracing) | `tools/demodreplay/scenarios/all-conditions.tropobeat.yml` |

The mock weather gives TERH a trapping duct, PROV a thin ducting layer, BOS normal air and IND no usable weather, so the same injection gives a different verdict at each. The README's [testing section](README.md#testing-the-correlator-and-demos) shows the commands.

## 14. Limits and open points

- **Thresholds are not calibrated.** `ber_factor`, `mer_drop_db` and the 3-report hysteresis are initial values. The test telemetry is one real day copied, which can't tell a channel that is marginal every day from one with a single bad day (rolling plan Q26).
- **Ducting resolution.** Weather-model levels are 200 m or more apart, so thin ducts can be missed and a channel's degradation reported as `dual_unexplained` (Q17). The ducting scores are not yet calibrated against real soundings (Q16).
- **Transmitters considered.** Only licensed full-power stations within 200 miles are co-channel paths (Q11).
- **Outages and frame faults** are not yet reported as such. The lock-loss guard only keeps them from being read as CCI. What to report when a whole site is lost *and* a path is ducting is open (Q22, Q25).
- **LRF code mismatches.** Sites whose demod LRF codes don't match the LRF list have no co-channel paths, so their channels are never confirmed (Q14).
- **Side labels.** Inputs that report no side are left out of pairing (Q23).
