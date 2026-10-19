# Plan: Phase 4 — RF telemetry integration (4a / 4b / 4c)

> **Status:** Phase 4a implemented in the Phase 4a commit (2026-10-19). Phases 4b and 4c are designed here but not built. This is the working plan as approved, kept for reference. For how the finished code behaves, see [README.md](../../README.md); for project status and open questions, see [ROLLING_PLAN.md](../../ROLLING_PLAN.md).
>
> **Where the 4a build differs from this plan:**
> - **Baseline window setting.** It's `baseline.window_days: 7` (whole days, an integer) rather than `window: 7d`, since Beats durations have no day unit.
> - **Inputs with no call sign.** Instead of being skipped, they get a baseline with status `no_station`, which is published (so spare inputs are visible) and never used by the rules.
> - **`baseline` metricset thresholds.** `ber.spike_threshold` and `mer.drop_threshold_db` are left out until 4b adds `ber_factor` and `mer_drop_db`; 4a publishes `ber.median`, `ber.effective` and `mer.median_db`.
> - **Ducting store.** It also has `LRFMNegative(lrf, at)` (any channel at the LRF), which the 4b outage rule needs. A channel with no co-channel paths is reported as known and not negative.
> - **RabbitMQ.** `amqp091-go` is pinned to v1.14.0 (the routelogbeat version; v1.16.0 was released the same day). The TLS keys are `ssl.enabled`, `ssl.ca_cert` and `ssl.insecure`, matching `tropobeat.elasticsearch`.
> - **Client location (§2).** The RabbitMQ client lives in `internal/rabbitmqclient`, not `beater/rabbitmqclient`. That follows the project convention that tropobeat's own packages live under `internal/`, alongside the other service clients, and keeps `config` from importing a package under `beater`. It is still created in `New()` and consumed in `Run()`.
> - **Exchange and routing key defaults.** The plan had no default exchange or routing key. They now default to `x.tropobeat.demod` and `demod.metrics` (user, 2026-10-19), so only the password is required when enabled.
> - **Run loop.** The for/select loop is the `consume()` method, called at the end of `Run()`.
> - **Limited run** on 2026-10-19 against a local RabbitMQ and the test cluster (TERH, PROV, BOS):
>   - 40 baselines; TERH side A, RF 10 matched the plan (9,884 reports, BER 0 → 1e-6, MER 33 dB).
>   - Replayed sample: 2,815 instances at 207 LRFs; only the 8 reports with no LRF ID were rejected.
>   - Reconnect checks: forced close, queue deletion, broker restart and a missing exchange all recovered.
>   - The queue held at 50,000 while tropobeat was stopped.
>   - The test cluster's index template predates the `baseline` fields, so they were mapped dynamically (e.g. `ber.median` as `long`). The template needs overwriting and the data stream rolling over.

## Context

Phases 1–3 produce co-channel paths and an hourly ducting analysis per path point. Phase 4 adds the live RF side: per-minute demod telemetry (MER, Pre-FEC BER, power, lock) compared with a 7-day baseline, then correlated with the ducting results to separate environmental co-channel interference from site outages. The user supplied the rules (pasted spec) and split the phase:

- **4a: foundation.** RabbitMQ ingest of live demod reports, 7-day baselines held in memory (and published), latest ducting results held in memory. A cache library was dropped as unnecessary (see New dependencies).
- **4b: engine.** Stress checks, 3-minute hysteresis, A/B dual-path confirmation against dM/dh < 0, and outage exclusion.
- **4c: output.** Events and alerts (details to be decided by the user later).

This session's deliverable: rewrite Phase 4 in ROLLING_PLAN.md as 4a/4b/4c, then **implement 4a**. 4b is designed here so 4a's data shapes fit it, and built in a later session.

### Facts from the 2026-10-13 demod sample (drive the design)
- The BER field is `rfdemod.rf_demod.d_pre_fec_ber` (double), not `f_pre_fec_ber` as in the spec. MER (`i_input_mer`) and power (`i_input_power`) are integers.
- BER is exactly 0 in 96% of locked reports, so most 7-day medians will be 0. **Decision: effective baseline = max(median, 1e-6), configurable.**
- BER 16.777215 (0xFFFFFF/1e6) appears 19 times: a saturated or invalid reading. Treat it as a breach, not as a value.
- 1,098 of 1,118 (LRF, channel) pairs have both sides A and B, so dual-path is feasible almost everywhere. Side labels also include `"1 A"` (42 reports) and missing (14 reports).
- Key collisions: 186 (LRF, side, channel) keys have 2–5 inputs. These are almost all channel 37 spare or `NA` inputs that are always Not Locked. Locked-only data has very few collisions.
- Not Locked reports carry MER 0 and power −80 (or 0/0). Six LRFs were entirely unlocked during the sample (UTC, HAR, SHRM, ZANE, MNKTO, PKRBG), which shows long-running outages exist.
- Each input reports about once a minute.

### Findings from the 7-day test data (10.9.0.69, reviewed 2026-10-19)
- There are 12 daily indices (Oct 11–22), not 7, and three are dated in the future. Each day is an identical copy: 3,989,938 docs, a complete 00:00–23:59 day, each input once a minute with about 2% of minutes missing. **The baseline query must have an upper bound** (`lt: now/d`). An open-ended `gte: now-7d` would pull in the future days.
- The baseline aggregation (composite + percentiles, 7 days, locked only) takes **16 s** over 4 pages and returns 2,249 keys. This confirms a daily refresh. 30 keys have fewer than 1,440 samples (the minimum is 7), so the `min_samples` rule is needed.
- BER median: 0 on 2,144 keys and above 1e-6 on 92. Many of those 92 are **channel 37 spare inputs reporting "Locked" with a saturated BER (16.777215)**. Fix: a baseline whose median is saturated is invalid, and keys with no call sign in the tag are excluded, as discovery already does.
- MER median: the TDigest estimate differs from the exact median on 29 keys. MER is an integer, so compute its median exactly from a `terms` histogram per key (cheap).
- One-day simulation of the 4b rules (no ducting gate, 3-minute hysteresis to enter):
  - BER spike (10× and the 1e-6 floor): 1,749 episodes a day on 145 keys. 57 keys have 10 or more a day (worst ROA B 13 with 147, then BUF 7 and LAFLA 16).
  - MER −3 dB: 1,115 episodes on 203 keys.
  - A+B together: 805 episodes on 87 of 1,105 pairs. BER-only A+B: 433 episodes on 52 pairs. A and B track each other almost exactly (EVN 22 has 40% of minutes on both), which suggests a shared antenna feed.
  - Whole-site unlock: only SANTB (15 minutes). The six always-down LRFs have no locked baseline, so the outage rule ignores them naturally.
- **The test data is one real day (2026-10-13) copied across 12 days.** The 7-day median therefore equals that day's median, and every count above is one day's behaviour. It **cannot** tell a channel that is marginal every day from one that had a single bad day (possibly a real event on 10-13, which then also sits inside its own baseline). Use this data to verify the mechanics (query, paging, cache, rule wiring and timing), not to tune thresholds or design handling for flapping channels. That calibration waits for real multi-day data (a new open question, linked to Q16).
- **A and B are one antenna feed split to two demods** (user, 2026-10-19). A+B agreement therefore shows the degradation is in the received RF rather than in a single demod. It is not independent path evidence, so in 4b the dM/dh < 0 check is the environmental evidence. The rolling plan and the 4b design must say this plainly.

---

## Phase 4a: foundation (implement now)

### New dependencies
- `github.com/rabbitmq/amqp091-go`: the official RabbitMQ client.
- **No cache library** (decided 2026-10-19; this supersedes "adopt a cache library").
  - Baselines are replaced in full once a day and ducting once an hour, and the live readings are owned by the 4b correlator.
  - Each store is therefore an immutable map built per refresh and swapped in with `atomic.Pointer`. That's the existing `snapshot` pattern: lock-free reads, always one complete map, and a failed refresh keeps the previous one.
  - Staleness is a timestamp check on read rather than a TTL.
  - Considered and rejected: ttlcache v3, which works on Go 1.23 but adds a mutex per lookup and a cleanup goroutine; Otter v2 and Ristretto v2 (need Go 1.24); gocache (needs Go 1.25, and is an abstraction over stores we don't use).

### 1. `internal/demod`: shared telemetry model (new package)
Move the demod field constants and `DefaultDemodIndex` here from [received.go](tropobeat/internal/cochannel/received.go#L16-L29). `cochannel` imports them, and the `LoadReceived` behaviour is unchanged.
- `Report`: LRF, Side, RFChannel, Frame/Slot/Input, Tag, Status, MER, PowerDBm, BER, Timestamp. JSON decoding matches the demod doc shape (`@timestamp` plus `rfdemod.rf_demod.*`), since the RabbitMQ messages are the same JSON.
- **`Instance{HostIP, Slot, Input}`** is the identity of one demod input at an LRF (user, 2026-10-19). `HostIP` is the top-level `host.ip` field; `Slot` and `Input` are `i_slot` and `i_input`. `Report.Instance()` returns it, and `Instance.String()` gives `10.91.114.129/9/2`. A report missing any of the three is rejected and counted.
- LRF, side and RF channel are **attributes** of a report, not its identity:
  - The LRF is upper-cased and trimmed.
  - Side normalisation maps `"1 A"`→`A`, and an empty side is kept as unknown, since the instance still identifies the input.
  - `Report.Channel()` returns `ChannelKey{LRF, RFChannel}`, used to group instances for A/B pairing and to look up ducting paths.
- Checked on the test cluster (7 days): 2,818 instances on 413 host IPs, 2,260 of them ever locked.
  - Every instance has exactly one LRF and one side.
  - 2,817 stay on one channel; one changed channel within the window.
  - 205 of 208 LRFs have two host IPs, one per frame (A and B).
  - The old (LRF, side, channel) key merged 11 pairs of distinct locked instances (e.g. OMA B 26 on 10.91.142.131 slot 9 input 4 and slot 10 input 4). Keying by instance removes those collisions.
- `Report.Locked()`, and `BERSaturated()` for BER ≥ 16.77.
- Tests decode a few lines from the sample tarball's shape (inline fixtures, as in existing tests).

### 2. `beater/rabbitmqclient`: AMQP consumer (new sub-package of beater, based on routelogbeat)
It lives under the beater package, as in routelogbeat. The client is **created in `New()`** and **consumed in `Run()`** (see section 6).
Reference: the `beater/rabbitmqclient` package and `rabbitmq_client` config in github.com/thetherington/routelogbeat (amqp091-go). Keep its strengths:
- one stable output stream that survives reconnects,
- an idempotent `Close`, with `amqp.ErrClosed` ignored,
- the full topology redeclared on every reconnect,
- the explanatory flow comment block.

Fix the gaps found in review:

| routelogbeat behaviour | Problem | Tropobeat version |
|---|---|---|
| Reconnects only on *connection* `NotifyClose` | A channel-only close (precondition failure, queue deleted, broker `basic.cancel`) leaves the connection up. `consumeLoop` then retries `Consume` on the dead `consumeCh` forever. | **A single supervisor loop** owns the conn and channel: connect → set up → consume, then `select` on ctx, conn `NotifyClose`, channel `NotifyClose`, `NotifyCancel` and deliveries closing. Any of these tears everything down, backs off and retries. No shared mutex and no polling. |
| Three goroutines coordinated through polled shared state | Hard to reason about. The races are benign but they exist. | One goroutine per consumer, stopped by `ctx`. |
| No `Qos` (prefetch); `autoAck: true` | The broker pushes without limit, and a crash loses whatever was in flight. | `Qos(prefetch)` with manual ack once the report is stored. Malformed bodies get `Nack(requeue=false)`. |
| `ExchangeDeclare` (active, fixed `direct`/durable) | If the inSITE-owned exchange exists with different properties, PRECONDITION_FAILED gives an endless reconnect loop. | `exchange_declare: passive` (default: check it exists, never create it), with `active` available for dev/replay. `exchange_type` and `durable` are settable. |
| Queue declared without arguments | A stopped beat accumulates an unbounded backlog (about 3.5M demod reports a day). | Queue arguments `x-message-ttl` (default 5m), `x-max-length` (default 50,000) and `x-overflow: drop-head`. |
| Fixed 5 s retry | Many beats hammer the broker in sync after an outage. | Exponential backoff (`reconnect_interval` 1s → `reconnect_max` 30s) with jitter. Reset after a stable connection. |
| `amqp://` only | No TLS. | `ssl` sub-config (enabled, CA, verification mode) → `amqps` with `DialTLS`. |
| Default password `abacus` in code | A secret in the source. | Defaults for `host` (localhost), `port` (8910), `vhost` (/), `username` (insite) and `queue` (`q.tropobeat.demod`). There is **no default password** and no default exchange; both are required when enabled. The password comes from the keystore or an env var, and is redacted in logs. |
| `ExchangeType`, `Durable` and `ReconnectInterval` not exposed in yml; `Persistent` unused | Config drift. | Every option is in `rabbitmq:`, with no publish-only options. |
| Global `logp.Warn` | Deprecated. | Named logger (`logger.Named("rabbitmq")`). |
| Reading the beater's `<-logsCh` after the stream closes | Spins on zero-value deliveries. | Keep the exposed delivery channel, but the beater's loop checks `ok` and stops reading a closed channel (sets it to nil). |

Additional items:
- Config keys match routelogbeat for operator familiarity: `host`, `port`, `vhost`, `username`, `password`, `exchange`, `queue`, `routing_key`. New keys: `enabled` (default false), `exchange_type` (default `direct`), `exchange_declare`, `prefetch` (500), `message_ttl`, `max_length`, `reconnect_interval`, `reconnect_max`, `ssl`. A connection name of `tropobeat` (`amqp.Config.Properties`) identifies tropobeat in the management UI.
- API:
  - `rabbitmqclient.New(cfg, logger) (*Client, error)` validates the config only. It does not dial, so `New()` never blocks on the broker.
  - `(*Client).Consume(ctx) <-chan amqp.Delivery` starts the supervisor goroutine and returns one stable channel, which stays open across reconnects and is closed when ctx is cancelled.
  - `(*Client).Reconnects() int64` is a counter for the ingest summary.
  - `Close()` is idempotent.
  - Acking and decoding are the caller's job (in `Run()`).
- Tests: the supervisor loop behind a small `dialer` interface with fakes, covering reconnect on conn close, channel close and cancel; backoff reset; Close/ctx shutdown; topology arguments; config defaults and validation (enabled without password or exchange → error).

### 3. Reference caches in the beater package (baselines, ducting)
The baseline and ducting caches live in `beater`, next to the jobs that fill them and on the `tropobeat` struct, the same way the `snapshot` is held today. New files: `beater/baseline.go` (store and loader) and `beater/ductingcache.go`, each with a `_test.go`. Fields on `tropobeat`: `baselines *baselineStore` and `ducting *ductingStore`, created in `New()`. Each gets a read method with the shape the 4b correlator will consume through an interface: `Lookup(demod.Instance, rfChannel int) (Baseline, bool)` and `MNegative(lrf, rf, at) (neg, known bool)`. That way 4b only adds the interfaces and wiring.
- **Baseline store** (`baselineStore`): `Baseline{Instance, RFChannel, LRF, Side, CardLabel, FrameLabel, Tags, MedianBER, MedianMER, Samples, From, To, RefreshedAt}`, keyed by `(Instance, RFChannel)`, plus `EffectiveBER()` = max(median, `ber_floor`). Each refresh builds a new `map[baselineKey]Baseline` with its `RefreshedAt`, and swaps it in via `atomic.Pointer`, mirroring the `snapshot` pattern in [jobs.go](tropobeat/beater/jobs.go#L19-L25). A failed refresh keeps the previous map. If the map is older than `max_age` (default 48 h, i.e. two missed refreshes), `Lookup` reports "no baseline" and a warning is logged. A key with fewer than `min_samples` reports (default 1,440 = 1 day) is marked insufficient.
- **Ducting store** (`ductingStore`): per path ID, the current-hour summary from the weather poll. Fields: per point class, min dN/dh and `MNegative` (dM/dh < 0, i.e. class `ducting`), plus the valid hour and poll time. Indexed by (LRF, RF channel) → path IDs, so 4b can go from a demod key to "any path on this channel has dM/dh < 0". Each weather poll builds a new map and swaps it in via `atomic.Pointer`. `MNegative` returns `known=false` when the map's poll time is older than 2 × `period`, so stale ducting can never confirm.

### 4. Baseline loader (`beater/baseline.go`: `bt.runBaseline` job → `loadBaselines(ctx, es, index, window)`)
An Elasticsearch composite aggregation over `demod_index`, written in the same style as `receivedQuery` (paged by `after_key`, with the query shown in the doc comment):
- filter: `@timestamp` from `now-7d/d` up to (but not including) `now/d`, `s_input_status: Locked`. The upper bound keeps out future-dated test indices.
- sources: `host.ip`, `i_slot`, `i_input` (the instance) and `i_rf_channel`. The channel is included so an input retuned within the window gets a separate baseline per channel instead of blending two stations.
- attribute sub-aggregations (`terms`, size 1): `s_lrf_id`, `s_demod_side`, `s_card_label` and `s_frame_label`
- sub-aggregations: `percentiles` (50, the median the spec asks for) on `d_pre_fec_ber` **with `"hdr": {"number_of_significant_value_digits": 3}`**. The default TDigest interpolates between BER's 1e-6 steps: on the test cluster it was within 10% of the exact median on only 61 of 95 non-zero keys (as much as 40% low), and returned tiny non-zero values on 13 keys whose median is 0. HDR matched the exact median on all 95, at the same speed (8.8 s vs 9.6 s for the full query). Then an exact MER median from a `terms` histogram on `i_input_mer` (integer dB, `size` 100, ordered by key); `min`/`max` on `@timestamp` (the data span, for the 7-day retention warning); `terms` on `s_input_tag` (size 5, for the call-sign check); plus `doc_count`.
- Verified on the test cluster (2026-10-19): for TERH A 10, 9,884 samples, BER median 0, MER median 33 dB (the cumulative count crosses 4,942 at 33), span Oct 12–18, tag `WTHI / WTHI2 (OTA)`. 225 ms for one LRF, about 16 s for the full network.
- A baseline is invalid when the BER median is saturated (≥ 16.77, e.g. spare channel‑37 inputs reporting Locked). Keys whose tags carry no call sign are skipped (reusing `cochannel.ParseCallSigns`).
- No key normalisation or collision handling is needed: each composite bucket is one (instance, channel). The LRF and side attributes are normalised as for live reports. When `lrf_sites` is set, a `terms` filter on `s_lrf_id` limits the query.
- The window is whole UTC days (`now-7d/d` to `now/d`), so results are stable within a day and Elasticsearch can serve repeats from its shard request cache.
- Runs at start-up, then daily on a gocron schedule (`baseline.schedule`, default `30 3 * * *`, after co-channel discovery). Optionally also at the 4b engine's request when it meets a new key with no baseline, rate-limited to once an hour. It uses the same `jobOptions` (singleton, logged errors) as [tropobeat.go](tropobeat/beater/tropobeat.go#L156-L208) and respects `lrf_sites`.
- Logs the keys loaded, the insufficient keys and the data span. It warns if the oldest data is under 7 days (retention check, new open question).

### 4b-pub. `baseline` metricset (published to the tropobeat index)
Every baseline run publishes one event per demod key through the beat output, as `metricset.name: baseline`. This works the same way as `cochannel`: a run ID, appended on every run, giving a dated daily snapshot, and consumers use the latest `run_id`. That's about 2,250 events a day. **It is a published record only** (user, 2026-10-19). Start-up always recomputes from the demod indices (about 9 s), so tropobeat never reads its own output index. The metricset is for dashboards, maps and 4b/4c evidence: an alert can cite the baseline it was judged against.
- `beater/events.go`: add `metricsetBaseline = "baseline"` and `baselineEvent(b Baseline, lrf *lrf.LRF, runID string, at time.Time) beat.Event`. The timestamp is the run time.
- Fields under `tropobeat.baseline`:
  - `run_id`
  - `instance.id` (`host_ip/slot/input`), `instance.host_ip` (`ip` type), `instance.slot`, `instance.input`, `card_label`, `frame_label`
  - `lrf.code`, `lrf.location` (geo_point, from the snapshot's LRF list, for maps), `demod_side`, `rf_channel`, `tags`, `call_signs`
  - `status`: `ok`, `insufficient` (fewer than `min_samples`) or `invalid` (saturated BER median). Keys that aren't `ok` are published too, so dashboards can show them.
  - `samples`, `window.start` and `window.end` (the first and last report), `window.days`
  - `ber.median`, `ber.effective` (with the floor applied), `ber.spike_threshold` (`ber_factor` × effective, for reference; 4b reads the factor from its config)
  - `mer.median_db`, `mer.drop_threshold_db`
- `_meta/fields.yml`: a `tropobeat.baseline` group with strict types. The BER fields are `double` (`float` loses 1e-6 steps), MER is `float`, and the window fields are `date`. Then regenerate `fields.go` and the docs with the existing mage targets, as for `ducting`.
- `ducting.publish`-style switch: `baseline.publish: true` (default).
- Tests: `baselineEvent` field shapes in `beater_test.go`, matching the existing `cochannelEvent` tests.

### 5. Ducting cache feed
In `runWeather` ([jobs.go](tropobeat/beater/jobs.go#L196)), after `analyseLocations`, build each complete path group's summary and swap it into `bt.ducting`. The data already exists in `locs`/`complete`, so no extra API calls are made. On start-up the cache is empty until the first poll. 4b treats "no ducting data" as "dM/dh < 0 not established".

### 6. Wiring and config
- [config.go](tropobeat/config/config.go): new sections `rabbitmq:` (`rabbitmqclient.Config`) and `baseline:` (`window` 7d, `schedule`, `ber_floor` 1e-6, `min_samples`, `max_age` 48h, `publish`). The `correlator:` section comes in 4b. Defaults and validation are covered by config_test.go.
- [tropobeat.go](tropobeat/beater/tropobeat.go):
  - **`New()`**: when `rabbitmq.enabled`, create `bt.rabbit, err = rabbitmqclient.New(c.RabbitMQ, logger.Named("rabbitmq"))`. A config error stops start-up, with the same `stopMockWeather` cleanup as the other error returns. Create `bt.baselines` and `bt.ducting`.
  - **`Run()`**: after `registerJobs()` and `scheduler.Start()`, replace the blocking `<-bt.ctx.Done()` with a cancellable for/select loop:
    ```go
    var deliveries <-chan amqp.Delivery // nil when RabbitMQ is off: that case never fires
    if bt.rabbit != nil {
        deliveries = bt.rabbit.Consume(bt.ctx)
    }
    stats := time.NewTicker(time.Minute)
    defer stats.Stop()
    for {
        select {
        case <-bt.ctx.Done():
            return nil
        case d, ok := <-deliveries:
            if !ok { // only after ctx is cancelled; stop selecting on it
                deliveries = nil
                continue
            }
            var r demod.Report
            if err := json.Unmarshal(d.Body, &r); err != nil || !r.Valid() {
                _ = d.Nack(false, false) // malformed: drop, don't requeue
                ingest.rejected++
                continue
            }
            _ = d.Ack(false)
            ingest.count(r) // 4a: decoded count, distinct keys, locked/unlocked
            // 4b inserts bt.correlator.Submit(r) before the Ack
        case <-stats.C:
            // 1-minute ingest summary: decoded, rejected, distinct keys/LRFs, reconnects
        }
    }
    ```
    The loop handles about 2,800 reports a minute (about 46 a second), so decoding inline is cheap. In 4a the decoded reports are only counted, which verifies the full ingest path (broker → queue → decode → ack) before the engine exists.
  - **`Stop()`**: `bt.cancel()` ends the loop and the supervisor (which closes the AMQP connection), then `bt.rabbit.Close()`, then the scheduler and publisher client as now.
- `tropobeat.yml`, `tropobeat.reference.yml` and `_meta` config: document the new sections (keep the RabbitMQ password out of examples, with an env var shown).

### 7. Dev tooling for verification
A small Go program, `tools/demodreplay` (`go run`), reads the sample tarball and republishes each report to a RabbitMQ exchange. It can rewrite timestamps to "now" and run at real or accelerated speed. It is used to verify 4a, and later the 4b rules.

---

## Phase 4b: correlation engine (designed now, built next session)

### Create the engine: `beater/correlator` (new sub-package, modelled on routelogbeat's `beater/correlator`)
Correlation happens **as data is collected**, so the live per-key state belongs to the correlator.
- **One actor goroutine owns all state.** It is started by `correlator.New(cfg, opts...)` and stopped by `Close()`. No locks, the same design as routelogbeat's Engine:
  ```
  run():
    ticker = clock.NewTicker(SweepInterval)
    for select:
      <-closed        -> return
      r := <-in       -> ingest(r): update the key's live state, evaluate the rules
      now := <-ticker -> sweep(now): expire silent keys, record missed cycles, site-wide checks
  ```
- **Live state is a plain map** (`map[demod.Instance]*instanceState`) owned by the actor: the latest reading and the hysteresis counters per instance. A secondary index `map[demod.ChannelKey][]demod.Instance` groups instances by (LRF, channel) for A/B pairing, and a per-LRF instance set supports the site-wide outage check. A single owner needs no locking or background purge. The sweep expires keys with no report for 3 minutes.
- API (mirrors routelogbeat):
  - `Submit(r demod.Report) bool` (blocking, with an `InputBuffer` of 1,024) and `TrySubmit`
  - `Events() <-chan Event` (state changes; 4c publishes them)
  - `Stats() Stats`
  - `Close()`
- **Injected dependencies:**
  - `WithBaselines(BaselineSource)` and `WithDucting(DuctingSource)`: interfaces matching the read methods 4a gives `baselineStore` and `ductingStore`.
  - `WithClock(Clock)`: a fake clock for tests.
- **Wiring:**
  - `New()` creates `bt.correlator`.
  - The `Run()` loop calls `bt.correlator.Submit(r)` before `Ack`; if it returns false, the report is nacked with requeue.
  - The loop adds `case ev := <-bt.correlator.Events()` (handed to 4c).
  - The ingest summary adds `correlator.Stats()`.
  - `Stop()` closes the correlator after the RabbitMQ client.
- **Config `correlator:`:**
  - timings: `sweep_interval` 10s, `stale_after` 90s, `expire_after` 3m, `input_buffer`
  - rules: `ber_factor` 10, `mer_drop_db` 3, `hysteresis_cycles` 3

### Rules
The rules run **inside the correlator actor as each report arrives** (`ingest`). The sweep handles time-based cases: missed cycles, silent keys, and site-wide checks for LRFs that stopped reporting. Each report for a key is that key's 1-minute polling cycle. Baselines and ducting come through the injected `BaselineSource` and `DuctingSource`. Per demod instance (`host.ip` + `i_slot` + `i_input`), against its own (instance, channel) baseline:
- **BER spike**: locked and (BER ≥ `ber_factor` × EffectiveBER, or saturated).
- **MER drop**: locked and MER ≤ median MER − `mer_drop_db`.
- **Environmentally Stressed**: a BER spike for 3 consecutive minutes (the spec's rule).
- **Hysteresis**: a per-key and per-LRF state machine. A state is entered after 3 consecutive breached cycles and cleared after 3 consecutive clean cycles. A cycle with no fresh reading neither counts as breached nor clean.
- **A+B pairing as data arrives:** when a report for an instance is ingested, the instances on the other side for the same (LRF, channel) are looked up through the channel index. Their latest state is used if it's no older than `stale_after`. A side counts as anomalous if any of its instances on that channel is (this matters for the 11 channels received twice on one frame).
- **Per-frame loss:** all instances on one `host.ip` unlocked at once (one frame down, the other fine) is its own state, distinct from a site outage. It is reported as an equipment or frame fault, not as environmental. That makes "the same cycle" mean within the same 90 s.
- **Confirmed environmental CCI (100%)**: since A and B share one feed, A+B agreement rules out a single-demod fault, and the ducting check supplies the environmental evidence. For (LRF, channel), side A and side B are both anomalous (BER spike or MER drop) in the same cycle, and the cached ducting for any path on (LRF, channel) has dM/dh < 0 at the valid hour. The 3-cycle hysteresis applies.
- **Outage exclusion**: every expected channel at the LRF (keys with a sufficient baseline, both sides) is unlocked or has collapsed power and MER, and no path on that LRF has dM/dh < 0. The LRF is flagged as an infrastructure (power or backhaul) outage, and environmental alerts for it are bypassed.
- The case where the whole site is lost *and* dM/dh < 0 is not covered by the spec. It goes to the open questions.

## Phase 4c: output (scope to be detailed by the user)
A placeholder in the rolling plan: state-change events, periodic baseline/state snapshots, and alert integration with inSITE. Options are listed for the user to choose from.

---

## ROLLING_PLAN.md update (customer-facing style: no versions, file names or function names; short library names are OK)
- Phase 4 is replaced by 4a (🔄), 4b (📋) and 4c (📋) tables with the items above. The old "Baselines by time of day and season" row is superseded by the 7-day median, and the "interference signature" row folds into 4b.
- Correct the field name in the demod telemetry row (`d_pre_fec_ber`).
- Architecture diagram: add the RabbitMQ demod stream → live telemetry and baselines → correlation.
- **Decisions**: live telemetry from a RabbitMQ exchange via tropobeat's own bounded queue; baselines and ducting held in memory as whole-map snapshots swapped on each refresh (no cache library: they are replaced daily and hourly, so expiry per entry adds nothing), with baselines rebuilt from Elasticsearch; a 7-day median per demod instance (host IP, slot, input) and channel; a 1e-6 BER floor; MER drop = 3 dB below the median; dM/dh < 0 taken as the ducting class at a path point for the current hour.
- **Open questions (Q20+)**: the RabbitMQ exchange name, routing key, credentials and TLS (who provides them); whether demod index retention is at least 7 days; what to do when the whole site is lost *and* dM/dh < 0; the `"1 A"` and missing side labels in source data; how to treat LRFs that are always down (the six in the sample); whether a demod that stops reporting entirely counts as an outage; threshold calibration and handling of flapping channels, which need real multi-day demod data (the test set is one day copied).
- **Decision** (2026-10-19): baselines are published daily as the `baseline` metricset in the tropobeat index, as a record for dashboards and evidence. Tropobeat always recomputes them from the demod telemetry and never reads them back.
- **Decision** (2026-10-19): the A and B demods share one antenna feed. A+B agreement rules out a single-demod fault, and dM/dh < 0 is the environmental evidence.
- Change log entry, and a "Last updated" bump.
- README: add a RabbitMQ and baseline section when 4a is complete.

## Verification
1. `go test ./...` in `tropobeat/`, with new unit tests for demod decode and key normalisation, the baseline query builder and response parsing (fixture JSON), store swap behaviour, staleness (`max_age`, 2 × `period`) and a failed refresh keeping the previous map, the consumer handle path, and config defaults.
2. Local RabbitMQ (`docker run rabbitmq:management`, with a broker restart, a queue deletion and a forced connection close from the management UI during a replay, to exercise every reconnect path) and the existing local Elasticsearch, with the sample loaded into a `log-metric-p-rfdemod-*` index.
3. Run tropobeat with `rabbitmq.enabled: true` and `lrf_sites` limited to a few sites, plus `go run ./tools/demodreplay`. Check the ingest summary log (decoded counts, keys about equal to the sample), the baseline log (keys loaded; the under-7-days warning fires on the 15-minute sample), and the ducting cache populated after the first weather poll (with `openmeteo.mock` to avoid API calls).
4. In the tropobeat index, check `metricset.name: baseline`: about 2,260 events (one per locked instance and channel) for the latest `run_id`, and TERH's side-A instance on RF 10 with `samples` 9,884, `ber.median` 0, `ber.effective` 1e-6 and `mer.median_db` 33. Channel‑37 spares should show `status: invalid`.
5. Stop tropobeat while replaying, and confirm the queue stays capped (max-length and TTL) in the RabbitMQ management UI.
