# Tropobeat dashboards

Tropobeat ships five Kibana dashboards:

- **[Tropobeat] Ducting Watch** shows tropobeat's ducting results. One LRF selector drives every panel: headline figures, a map of the paths that are ducting, heat maps of the network and of each path over the last 72 hours, where along a path the ducting is, and the duct detail behind it.
- **[Tropobeat] Co-channel paths** checks that co-channel discovery has run. It puts every co-channel path, midpoint and transmitter from the latest discovery run on one map, together with every LRF.
- **[Tropobeat] Correlation debug** shows how the correlation engine reached its verdicts: the latest verdict per LRF and channel, verdicts over time, the decision-trace reasons, each A/B pair decision with its explanation, MER and BER against their thresholds, and every trace record.
- **[Tropobeat] CCI operations** is the operator's view of co-channel interference: headline figures, a map of open episodes with each likely interferer's path and station, a card per open episode, ducting and episodes by hour, episodes by region, verdicts by LRF hour by hour, the top likely interferers, episodes per day and data health.
- **[Tropobeat] Episode timeline** shows how one episode was made: pick it from a list of the last 30 days, and every input report, hysteresis change, ducting lookup, weather poll and pair decision is laid out on one time axis, with the decision log beneath.

What the states, scores and thresholds mean is in [DUCTING.md](DUCTING.md); the correlation verdicts and trace reasons are in [CORRELATION.md](CORRELATION.md). Ducting Watch repeats the essentials in its **Ducting reference** panel.

## Contents

1. [Installing](#1-installing)
2. [Ducting Watch](#2-ducting-watch)
3. [Co-channel paths](#3-co-channel-paths)
4. [Correlation debug](#4-correlation-debug)
5. [CCI operations](#5-cci-operations)
6. [Episode timeline](#6-episode-timeline)
7. [Colours](#7-colours)
8. [Limitations](#8-limitations)
9. [Changing the dashboards](#9-changing-the-dashboards)

---

## 1. Installing

**Requirements:**

- Kibana 9.4 or later (built and tested on 9.4.2).
- Tropobeat events in indices matching `tropobeat-*`.
- For **CCI operations**, the Magnum Analytics visualizations plugin (inSITE's Kibana), whose Handlebars visualization draws the KPI tiles and the open-episode cards. Without it those panels show an error; the other panels still work. **Episode timeline** uses core Kibana only.
- The LRF list loaded with `tropobeat insert lrfcoordinates` ([README](README.md#tropobeat-insert-lrfcoordinates-lrf-sites)). It creates `lrf-coordinates` in lookup mode, which the Ducting Watch network heat map joins for each LRF's city and state. An `lrf-coordinates` created by an earlier version isn't in lookup mode; recreate it once with `--recreate`.

There are two ways to install the dashboards. Both create the same objects:

| Object | ID |
|---|---|
| Dashboard **[Tropobeat] Ducting Watch** | `tropobeat-ducting-watch` |
| Map **[Tropobeat] Ducting Watch map** | `tropobeat-ducting-watch-map` |
| Markdown panel **[Tropobeat] Ducting reference** | `tropobeat-ducting-reference` |
| Dashboard **[Tropobeat] Co-channel paths** | `tropobeat-cochannel-paths` |
| Map **[Tropobeat] Co-channel paths map** | `tropobeat-cochannel-paths-map` |
| Dashboard **[Tropobeat] Correlation debug** | `tropobeat-correlation-debug` |
| Markdown panel **[Tropobeat] Correlation reference** | `tropobeat-correlation-reference` |
| Saved search **[Tropobeat] Correlation trace** | `tropobeat-correlation-trace` |
| Dashboard **[Tropobeat] CCI operations** | `tropobeat-cci-operations` |
| Map **[Tropobeat] CCI operations map** | `tropobeat-cci-operations-map` |
| Handlebars **[Tropobeat] CCI KPI: …** (6 tiles) | `tropobeat-kpi-active-cci`, `tropobeat-kpi-needs-a-look`, `tropobeat-kpi-at-risk`, `tropobeat-kpi-episodes-24h`, `tropobeat-kpi-median-duration`, `tropobeat-kpi-weather` |
| Handlebars **[Tropobeat] CCI open episodes** | `tropobeat-cci-open-episodes` |
| Vega **[Tropobeat] CCI ops: …** (6 charts) | `tropobeat-ops-trends`, `tropobeat-ops-episodes-by-region`, `tropobeat-ops-verdict-heatmap`, `tropobeat-ops-top-interferers`, `tropobeat-ops-episodes-per-day`, `tropobeat-ops-data-health` |
| Dashboard **[Tropobeat] Episode timeline** | `tropobeat-episode-timeline` |
| Vega **[Tropobeat] Episodes: list, 30 days** and **timeline** | `tropobeat-episode-list`, `tropobeat-episode-timeline-lanes` |
| Saved search **[Tropobeat] Episode decision log** | `tropobeat-episode-decision-log` |
| Data view `tropobeat-*` | `tropobeat-*` |
| Data view **LRF sites (tropobeat)** on `lrf-coordinates` | `tropobeat-lrf-coordinates` |

**With tropobeat (recommended).** Set `setup.kibana` in `tropobeat.yml`, then:

```bash
./tropobeat setup --dashboards -e
```

Installed packages carry the dashboards in tropobeat's `kibana` directory, where setup finds them. From a source checkout, point it at the assets in the repository:

```bash
./tropobeat setup --dashboards -e \
  -E setup.kibana.host=https://kibana.example:5601 \
  -E setup.dashboards.directory=_meta/kibana
```

Add `-E setup.kibana.space.id=<space>` to load into a Kibana space other than the default. Loading again replaces the objects, so any changes made in Kibana are lost (see [Changing the dashboards](#9-changing-the-dashboards)). Setup also creates the `tropobeat-*` data view with every tropobeat field. Kibana keeps a saved object's ID unique across spaces: loading into a second space gives the objects new IDs there, which the dashboards don't depend on.

**By hand.** In Kibana, go to **Stack Management › Saved objects › Import** and choose `_meta/kibana/ducting-watch.ndjson`, `_meta/kibana/cochannel-paths.ndjson`, `_meta/kibana/correlation-debug.ndjson`, `_meta/kibana/cci-operations.ndjson` or `_meta/kibana/episode-timeline.ndjson`. Each file holds one dashboard and everything it uses, including a minimal `tropobeat-*` data view; Kibana reads the fields from the index.

## 2. Ducting Watch

### Panels

All panels read the tropobeat indices. "Latest poll" means the most recent weather poll within the dashboard's time range.

| Panel | Shows | Time range |
|---|---|---|
| **LRFs with a trapping duct** | LRFs where at least one path is trapping at the latest poll, with the number of paths polled | Dashboard |
| **Paths ducting now** | Paths at level 2 or 3 at the latest poll, and their share of all paths | Dashboard |
| **Selected LRF now** | The highest state at the latest poll, with counts of trapping and ducting paths. With no LRF picked it covers every LRF. | Dashboard |
| **Worst path now** | The path with the highest state, then the highest index: transmitter (its site label for a multi-site station), channel and LRF, with its state, index and distance | Dashboard |
| **Map** | Paths that are **ducting or trapping** at the latest poll, with their midpoints and transmitters, coloured and sized by the path ducting index. Paths at normal or super-refractive are left off. Every LRF in view is shown. Hovering a path, midpoint or transmitter shows its path ID, call sign, site label, channel, state, level and index. | Dashboard |
| **Co-channel paths table** | Every path at the latest poll, worst first: LRF, transmitter (its site label for a multi-site station), channel, band, distance, state, level, index, and the score at the LRF, midpoint and transmitter | Dashboard |
| **Network heat map** | The highest ducting state of each LRF, hour by hour (top 20 LRFs). Each LRF is labelled with its site's city and state, for example `BOS · Andover, MA`. | Fixed: last 72 h |
| **Path heat map** | The ducting state of each co-channel path, hour by hour (top 50 paths) | Fixed: last 72 h |
| **Ducting reference** | States, gradient classes, the path index and trapping thickness, as a reminder | None |
| **Where on the path** | The score at the LRF, midpoint and transmitter over time, with the 0.6 ducting line | Fixed: last 72 h |
| **Duct detail** | For each weather location at the latest poll: its role (LRF, midpoint, transmitter), path, state, class, score, steepest gradient, duct type, base and top, thickness, strength (ΔM) and how many paths use it | Dashboard |

The distance in the table and the worst-path panel comes from the daily co-channel discovery run. If the time range doesn't include a run, the distance is left blank.

### Using it

- **Pick an LRF or band** with the controls at the top. Every panel follows, including the map.
- **Pick a time** with the time slider. The figures, map and tables move to the latest poll in the slider's window. The heat maps and **Where on the path** stay on the last 72 hours, so the slider can't narrow them.
- **Pick a path**, which fills **Where on the path** and narrows **Duct detail** to that path's three points:
  - click a cell in the path heat map, or
  - click a path on the map, then the ⊕ (filter) button next to `tropobeat.path_ids` in its tooltip.
- **Clear a pick** by removing its filter pill under the query bar.

Without a path picked, **Where on the path** shows the average score of every path in view, which is only a rough picture. **Duct detail** lists every location, labelled with all the roles and paths it serves (for example `TERH:11204:18 +21 more` for an LRF point).

## 3. Co-channel paths

One map of the **latest discovery run** in the time range (the last 7 days by default, so the daily run is always included), with an **LRF** picker. Earlier runs are not drawn.

| Layer | Drawn as | Hover shows |
|---|---|---|
| **Co-channel paths** | Lines coloured by band: UHF teal, high VHF blue, low VHF pink | LRF, transmitter, site (the call sign, or the site label for a multi-site station), FCC facility ID, RF channel, band, distance (mi), bearing (°), ERP (kW), service, the LRF's own stations, path ID, discovery run |
| **Midpoints** | Small pale-grey dots | The same as the path |
| **Transmitters** | Pink map markers; call signs from zoom level 7 | The same as the path |
| **All LRFs** | Grey squares labelled with code, city and state, for example `BOS · Andover, MA` (as on the Ducting Watch network heat map) | Street, city, state, ZIP |
| **LRFs with paths** | White squares on top of the grey ones | Number of paths and distinct transmitters, the RF channels with paths, discovery run |

**Reading it.** An LRF with a white square has co-channel paths from the latest run; the tooltip gives the count and channels. A grey square without a white one is an LRF with no co-channel paths: either none of its channels has a same-channel transmitter within 200 miles, or it had no locked demod channels when discovery ran.

The **LRF** picker narrows the paths, midpoints, transmitters and white squares. **All LRFs** always shows every site, for context.

## 4. Correlation debug

For checking and explaining the correlation engine: why a channel was or wasn't reported as co-channel interference. It reads the `correlation` verdict events and the `correlation_trace` decision-trace records ([CORRELATION.md](CORRELATION.md#10-tracing-a-decision)). It opens on the last hour and refreshes every 30 seconds.

**Controls:** LRF, RF channel, trace category and trace reason. Pick an LRF and channel to follow one A/B pair through every panel.

### Panels

| Panel | Shows |
|---|---|
| **Verdicts: latest event per LRF and channel** | The current verdict of every LRF and channel that has had one, worst first: the event kind, the reason, the strength (ducting or trapping), the likely interfering transmitter (its site label for a multi-site station) and its distance, each side's view, and how long the episode has lasted. |
| **Correlation reference** | The verdicts, the trace categories and decisions, and where to look when a channel didn't alert. |
| **Verdict per LRF and channel** | A heat map of each LRF and channel's verdict over time, from 0 (clear) to 5 (cci_confirmed), with about 75 columns across the selected time range. Shows when episodes started, how long they lasted and how they changed. It is filled from the verdict events and the once-a-minute `pair` trace records, so with `pair` tracing turned off an open episode only shows every 5 minutes (its ongoing events). Pairs that stay clear don't appear. |
| **Decision trace: reasons over time** | Trace records per minute, stacked by reason. A burst of one reason (for example `other_side_stale`) points straight at the cause. |
| **Verdict events by verdict** | Verdict events per minute, by verdict (start, update, ongoing and clear events together). |
| **Pair decisions: latest per LRF and channel** | The latest A/B decision for each LRF and channel: decision, reason, verdict, each side's view and anomalies, the strongest path, and the one-line explanation. |
| **MER vs threshold by side** | The MER of each side against its drop threshold. Needs the `input` trace category. Pick one LRF and channel. |
| **Pre-FEC BER vs spike threshold by side** | The same for BER, on a log scale. |
| **Trace records** | The newest 500 trace records with their explanations. |
| **Trace records in Discover** | The **[Tropobeat] Correlation trace** saved search, for sorting, filtering and opening records in Discover. |

### Using it

- **A channel didn't alert.** Pick its LRF and channel. **Pair decisions** gives the reason in words. If there's no pair record at all, neither input stayed anomalous for 3 reports in a row. Turn on the `input` category for that LRF (`correlator.trace.lrf_sites`) and read the **MER** and **BER** panels against their thresholds.
- **A channel did alert.** Its **Verdicts** row names the likely interferer; the matching **Pair decisions** and **Trace records** show both sides' readings and the path that confirmed it. Every record of one episode shares its episode ID.
- **Is the correlator seeing everything?** Set the category control to `skip` (inputs that can't be judged) or `sweep` (inputs live, pairs not clear, trace records dropped). Both need those categories turned on.

The MER and BER panels and the `skip`, `ducting` and `sweep` records need their trace categories enabled; by default only `pair` and `hysteresis` are traced.

## 5. CCI operations

For the operators watching for co-channel interference. It reads the `correlation` verdict events, the `weather` polls and `lrf-coordinates`. It opens on the last 24 hours and refreshes every minute. Times are in the browser's local time, which is Kibana's default (`dateFormat:tz` set to Browser).

An episode is open while its latest event is under 6 minutes old and isn't a clear (an open episode publishes at least every 5 minutes). Channels are named by the station the LRF receives on them, with the LRF and RF channel beside it.

### Panels

| Panel | Shows |
|---|---|
| **KPI tiles** | Active CCI (channels and LRFs, trapping and ducting; the tile pulses while any CCI is open), needs a look (open episodes that aren't CCI, by verdict), at risk (LRFs and paths ducting or trapping in the latest weather poll), episodes in 24 hours, median and longest duration of the episodes that cleared, and the latest weather poll. |
| **Map** | Every LRF, the paths ducting in the latest poll (faint), LRFs with ducting paths, open episodes coloured by verdict, and for each open CCI episode the likely interferer's path (red) and station (tower icon, with its call sign). |
| **Open episodes** | A card per open episode, worst first: the station, LRF and channel, the verdict and strength, the likely interferer and its distance (or what to check when it isn't CCI), the power signature, the duration at the last event and the start. CCI cards pulse. |
| **Ducting and episodes, by hour** | Paths ducting or trapping, and open episodes stacked by verdict, per hour over the dashboard's time range. |
| **Episodes by region** | Episodes in the time range by ducting climate (Pacific coast, Mountain West, Interior, Great Lakes, Atlantic coast, Gulf coast) or by US time zone (click the toggle), with an outer ring splitting each region by peak verdict. The hover text says why ducting happens in each climate region. |
| **Verdicts by LRF, hour by hour** | The worst verdict each LRF had in each hour, for the LRFs that had anything other than clear. |
| **Top likely interferers, 7 days** | The stations named on confirmed-CCI events, by hours as the likely interferer. |
| **Episodes per day, 14 days** | Episodes by the local day they started, today highlighted, against the average of the complete days. |
| **Data health** | Correlation sweeps, the latest weather poll, baselines, co-channel discovery and paths without usable weather, each with a status dot. |

### Using it

- **Something is open.** The open-episode cards say which station and what to do: for CCI, the likely interferer and how far away it is; for the other verdicts, what to check. The map shows the interfering path and station.
- **Is it seasonal or local?** **Episodes by region** and **Verdicts by LRF** show where episodes cluster; **Ducting and episodes, by hour** shows whether they follow the duct.
- **To see how an episode was made**, open it in **Episode timeline**.

## 6. Episode timeline

For explaining one episode: what the telemetry reported, when each side became degraded, what the ducting was, and what the correlator decided, in order. It reads the `correlation` events, the `correlation_trace` records and the `weather` polls.

### Panels

| Panel | Shows |
|---|---|
| **Episodes, last 30 days** | Every episode, newest first, with open episodes pinned at the top. Each row: start, station with LRF and channel, peak verdict, duration, likely interferer, each side's card, frame and input, and the episode ID. Scroll with the mouse wheel or the scroll bar. **Click an episode** to point the whole dashboard at it. |
| **Timeline** | One time axis from 5 minutes before the episode's start to 3 minutes after its clear: the verdict, the published events, each side's BER per report against its threshold (B1–B3 count the anomalous reports into degraded, C1–C3 the clean ones out), each side's state (counting, degraded, recovering, stressed, frame down), MER against its threshold, ducting (the correlator's lookups, and the strongest path's weather polls beneath), hysteresis changes and pair decisions. Hover anywhere for every value at that moment. |
| **Episode decision log** | Every event and trace record of the episode, oldest first. |

### Using it

- **Clicking an episode** replaces the dashboard's filters with one for that LRF and channel and the episode's time window, and sets the time range to the same window. The window keeps the reports that led up to the episode, which carry no episode ID.
- **Saturated BER** (the demod reports 16.777215 when it can't measure) is drawn as a full-height outlined bar marked SAT.
- **Frame down** marks a side whose lock loss came with its whole frame unlocked: a frame or site problem the correlator doesn't count as signal degradation.
- Decisions made within 5 seconds of each other belong to one sweep (the two sides' reports land moments apart); the timeline shows the last of them, and its tooltip names the one before. The decision log keeps them all.

## 7. Colours

| Level | State | Heat maps | Table cells |
|---|---|---|---|
| 0 | Normal | Faint teal | None |
| 1 | Super-refractive | Soft yellow | Yellow, 40 % opacity |
| 2 | Ducting | Neon orange | Orange, 40 % opacity |
| 3 | Trapping | Rose-red | Rose-red, 40 % opacity (State column only) |

In the Ducting Watch table, the LRF, midpoint and transmitter score cells are coloured by the class the score falls in: 0 normal, above 0 to 0.6 super-refractive, 0.6 and above ducting. Trapping depends on the path's channel, so it appears only in the State column.

The 0.6 boundary in the table colours, the chart's reference line and the **Ducting reference** panel follow tropobeat's score bands. Update them if the bands are recalibrated ([rolling plan](ROLLING_PLAN.md#4-open-questions), Q16).

## 8. Limitations

- **The network heat map needs `lrf-coordinates` in lookup mode.** Otherwise the panel shows an error. `tropobeat insert lrfcoordinates` creates it that way; an older index needs `--recreate` once.
- **The network heat map can't pick an LRF by clicking**, because its labels are built in the query and aren't a field. Use the **LRF** control. The path heat map can still pick a path by clicking.
- **Picking a path takes two clicks on the map.** Kibana Maps filters from a tooltip button; filtering on a single click needs a drilldown, which needs a paid Elastic licence.
- **Index names.** Many panels and map layers are ES|QL queries that read `FROM tropobeat-*` or join `lrf-coordinates`. If those names change, edit the queries as well as the data views.
- **Map layers draw up to 10,000 rows.** On the Co-channel paths map with every LRF in view, the full network may exceed that; pick an LRF.
- **Time range and polling.** Ducting Watch opens on the last 2 hours. With hourly polls, a shorter range can briefly hold no poll, and the latest-poll panels go blank.
- **MER and BER on Ducting Watch** are not shown yet; the Correlation debug dashboard shows them from the decision trace.
- **CCI operations needs the Magnum Analytics plugin** for its KPI tiles and open-episode cards; without it those panels show an error.
- **Episode timeline's per-report lanes need the `input` trace category**, and the ducting lookups need `ducting`; by default only `pair` and `hysteresis` are traced. Without them the BER, MER and state lanes fall back to the values on the episode's events (every 5 minutes) and say so. With `input` on, limit it to the LRFs of interest (`correlator.trace.lrf_sites`).
- **Episode timeline's weather row** looks up to 3 hours back for the poll in force when the window opens, which covers hourly polls.
- **Local time.** The Vega panels on CCI operations and Episode timeline use the browser's time zone. If Kibana's `dateFormat:tz` is set to a fixed zone, the other panels follow it and these don't.
- **Correlation debug panels** query `tropobeat-*` with ES|QL, like Ducting Watch. The verdict heat map's columns widen with the time range (1 minute over the last hour, 30 minutes over a day), so a short episode can share a column with clear minutes; narrow the range to see it minute by minute.

## 9. Changing the dashboards

The repository holds the dashboards as Kibana saved objects under `_meta/kibana/7/` (one file per object, which `tropobeat setup` reads) and as one bundle per dashboard in `_meta/kibana/*.ndjson`. To change one:

1. Edit and save the dashboard in Kibana.
2. **Stack Management › Saved objects:** select the dashboard and **Export** with **Include related objects**.
3. Replace the matching files in `_meta/kibana/7/` (`dashboard/`, `map/`, `visualization/`, `search/`, `index-pattern/`) and its `.ndjson` bundle with the exported objects. Keep the IDs above. References to the tropobeat data view must stay `tropobeat-*`, the data view that setup creates.
4. Run `make update` (or copy `_meta/kibana` into `_meta/kibana.generated`), which is what packages install.

Kibana remembers unsaved edits in the browser, per dashboard, and shows them over the saved version. After reloading a dashboard from the repository, use **Reset** if Kibana says it has unsaved changes.
