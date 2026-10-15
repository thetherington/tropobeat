package beater

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/elastic/beats/v7/libbeat/beat"

	"github.com/thetherington/tropobeat/internal/cochannel"
	"github.com/thetherington/tropobeat/internal/ducting"
	"github.com/thetherington/tropobeat/internal/lrf"
	"github.com/thetherington/tropobeat/internal/openmeteo"
)

// snapshot is the result of the latest discovery run. The LRF list and the
// paths built from it are always swapped together.
type snapshot struct {
	RunID string
	At    time.Time
	LRFs  []lrf.LRF
	Paths []cochannel.Path
}

// loadLRFs reads the LRF list and applies the lrf_sites filter. It returns
// the full list (to recognise demod IDs that belong to LRFs filtered out)
// and the selected LRFs.
func (bt *tropobeat) loadLRFs(ctx context.Context) (all, selected []lrf.LRF, err error) {
	// Read every LRF site from Elasticsearch. An empty index is an error:
	// without sites there is nothing to discover or poll.
	all, err = lrf.Load(ctx, bt.es, bt.config.LRFIndex)
	if err != nil {
		return nil, nil, fmt.Errorf("loading LRF sites from index %s: %w", bt.config.LRFIndex, err)
	}
	if len(all) == 0 {
		return nil, nil, fmt.Errorf("no LRF sites found in index %s; load them with `tropobeat insert lrfcoordinates`", bt.config.LRFIndex)
	}

	// Narrow the list to the codes in lrf_sites (all sites when it is
	// empty). Codes that don't match any site are only warned about, but
	// if nothing at all matches the run can't continue.
	selected, unknown := lrf.Select(all, bt.config.LRFSites)
	if len(unknown) > 0 {
		bt.logger.Warnf("lrf_sites: codes not found in index %s: %s", bt.config.LRFIndex, strings.Join(unknown, ", "))
	}
	if len(selected) == 0 {
		return nil, nil, fmt.Errorf("lrf_sites: none of %v are in index %s", bt.config.LRFSites, bt.config.LRFIndex)
	}
	if len(bt.config.LRFSites) > 0 {
		bt.logger.Infof("Limited to %d of %d LRFs (lrf_sites)", len(selected), len(all))
	}
	return all, selected, nil
}

// runCochannel re-reads the LRF list, reads each LRF's received channels from
// the demod telemetry, discovers the co-channel paths, swaps them into
// memory and publishes them. On error the previous snapshot is kept.
func (bt *tropobeat) runCochannel(ctx context.Context) error {
	// The weather job waits for the first discovery run so it has paths to
	// poll. Kick it off when this run returns, whether or not it succeeded;
	// startWeatherOnce only does this the first time.
	defer bt.startWeatherOnce()

	// The run ID is the UTC start time; it tags every event from this run
	// so co-channel and weather events can be tied back to it.
	start := time.Now().UTC()
	runID := start.Format("20060102T150405Z")

	all, selected, err := bt.loadLRFs(ctx)
	if err != nil {
		return err
	}

	// Find which RF channels each LRF is currently receiving, from demod
	// telemetry within the configured window, then drop the ones that
	// belong to LRFs outside lrf_sites.
	received, err := cochannel.LoadReceived(ctx, bt.es, bt.config.DemodIndex, bt.config.Cochannel.DemodWindow)
	if err != nil {
		return err
	}
	received = filterReceived(received, all, selected, len(bt.config.LRFSites) > 0)

	// For each received channel, search the FCC transmitter index for other
	// stations on the same channel within the radius of the LRF. Each one
	// becomes a co-channel path (LRF -> midpoint -> transmitter).
	res, err := cochannel.Discover(ctx, bt.es, cochannel.Options{
		FCCIndex: bt.config.FCCIndex,
		Radius:   bt.config.Cochannel.Radius,
	}, selected, received)
	if err != nil {
		return err
	}

	// Swap the new LRFs and paths in atomically. Only reached on success,
	// so a failed run leaves the weather job using the previous snapshot.
	bt.state.Store(&snapshot{RunID: runID, At: start, LRFs: selected, Paths: res.Paths})

	// Publish one event per discovered path.
	events := make([]beat.Event, len(res.Paths))
	for i, p := range res.Paths {
		events[i] = cochannelEvent(p, runID, start)
	}
	bt.client.PublishAll(events)

	// Everything below is logging: a summary of the run, then the channels
	// that were skipped and why.
	lrfsWithChannels := map[string]struct{}{}
	for _, r := range received {
		lrfsWithChannels[r.LRFCode] = struct{}{}
	}
	bt.logger.Infof("Co-channel discovery %s: %d LRFs, %d with demod channels, %d channels searched, %d own-station transmitters excluded, %d co-channel paths published (%s)",
		runID, len(selected), len(lrfsWithChannels), res.Channels, res.Excluded, len(res.Paths), time.Since(start).Round(time.Millisecond))

	// Demod IDs with no matching LRF site (likely missing from the LRF index).
	if len(res.UnknownLRFs) > 0 {
		bt.logger.Warnf("Co-channel discovery: demod LRF IDs not in %s (skipped): %s", bt.config.LRFIndex, strings.Join(res.UnknownLRFs, ", "))
	}
	// Channels whose matched own station is further from the LRF than the
	// configured limit; worth a warning as the demod tag or LRF location
	// may be wrong.
	for _, n := range res.TooFar {
		bt.logger.Warnf("Co-channel discovery: skipped %s RF %d: %s (tags %q)", n.LRFCode, n.RFChannel, n.Detail, n.Tags)
	}
	// Channels whose demod tags name no station (e.g. "Spare", "NA"). These
	// are expected, so they're logged at info rather than as a warning.
	for _, n := range res.NoCallSign {
		bt.logger.Infof("Co-channel discovery: skipped %s RF %d: %s (tags %q)", n.LRFCode, n.RFChannel, n.Detail, n.Tags)
	}
	// Channels whose own station isn't in the FCC full-power data (e.g. a
	// low-power or translator station). These are still searched, but
	// nothing is excluded, so the count is logged and the detail is debug.
	if len(res.NotInFCC) > 0 {
		bt.logger.Infof("Co-channel discovery: %d channels receive stations outside the FCC full-power set; nothing excluded on those", len(res.NotInFCC))
		for _, n := range res.NotInFCC {
			bt.logger.Debugf("Co-channel discovery: %s RF %d: %s (tags %q)", n.LRFCode, n.RFChannel, n.Detail, n.Tags)
		}
	}
	return nil
}

// filterReceived keeps the received channels of the selected LRFs. Demod IDs
// that are not in the LRF list at all are kept (so Discover reports them)
// unless lrf_sites is limiting the run.
func filterReceived(received []cochannel.Received, all, selected []lrf.LRF, limited bool) []cochannel.Received {
	// Map every known LRF code to whether it is selected. A code missing
	// from the map is unknown. Codes are upper-cased to match the demod
	// IDs, which LoadReceived upper-cases.
	known := map[string]bool{}
	for _, l := range all {
		known[strings.ToUpper(l.Code)] = false
	}
	for _, l := range selected {
		known[strings.ToUpper(l.Code)] = true
	}

	// received[:0:0] is an empty slice with zero capacity, so appending
	// allocates a new array instead of overwriting the caller's slice.
	out := received[:0:0]
	for _, r := range received {
		// Keep selected LRFs. Keep unknown IDs only for an unlimited run,
		// so they show up in the "not in index" warning; with lrf_sites
		// set they're most likely just sites outside the selection.
		isSelected, isKnown := known[r.LRFCode]
		if isSelected || (!isKnown && !limited) {
			out = append(out, r)
		}
	}
	return out
}

// runWeather fetches the weather for every co-channel path point and every
// LRF without paths, runs the ducting analysis once per unique location, and
// publishes one weather event per complete group plus the ducting events
// selected by ducting.publish.
func (bt *tropobeat) runWeather(ctx context.Context) error {
	// Use the latest snapshot from co-channel discovery. Before the first
	// successful run there is nothing to poll.
	snap := bt.state.Load()
	if snap == nil || len(snap.LRFs) == 0 {
		return errors.New("no LRF sites loaded yet")
	}

	// Build the weather groups. A group is the set of points whose
	// weather is fetched together and published as one event.
	//
	// Each co-channel path is a group of three points (LRF, midpoint,
	// transmitter), keyed by path ID. paths maps that ID back to the path
	// so the event can include it, and withPaths records which LRFs
	// already have at least one path.
	paths := make(map[string]*cochannel.Path, len(snap.Paths))
	withPaths := map[string]struct{}{}
	groups := make([]openmeteo.Group, 0, len(snap.Paths)+len(snap.LRFs))
	for i := range snap.Paths {
		p := &snap.Paths[i]
		g := p.Group()
		paths[g.ID] = p
		withPaths[p.LRFCode] = struct{}{}
		groups = append(groups, g)
	}
	// LRFs with no paths still get their own weather: a one-point group
	// keyed by the LRF code. lrfOnly records those group IDs.
	lrfOnly := map[string]string{}
	for _, l := range snap.LRFs {
		code := strings.ToUpper(l.Code)
		if _, ok := withPaths[code]; ok {
			continue
		}
		lrfOnly[code] = code
		groups = append(groups, openmeteo.Group{ID: code, Points: []openmeteo.Point{{
			Label:      cochannel.LabelLRF,
			Coordinate: openmeteo.Coordinate{Lat: l.Latitude, Lon: l.Longitude},
		}}})
	}

	// Count the points and how many distinct locations they reduce to.
	// These are only for the summary log: GroupProfiles does the actual
	// de-duplication, so an LRF shared by several paths is fetched once.
	var coords []openmeteo.Coordinate
	for _, g := range groups {
		for _, p := range g.Points {
			coords = append(coords, p.Coordinate)
		}
	}
	unique := openmeteo.CountUnique(coords, bt.config.OpenMeteo.CoordinatePrecision)
	batch := bt.config.OpenMeteo.BatchSize
	if batch <= 0 {
		batch = openmeteo.DefaultConfig.BatchSize
	}

	// Fetch the weather for all groups. On a partial failure fetchErr is
	// set, but the groups that did fetch are still returned and published;
	// the error is returned at the end.
	start := time.Now()
	results, fetchErr := bt.weather.GroupProfiles(ctx, groups, start)

	// Keep only complete groups, so a path event always has all three
	// points. Work out each group's LRF code: from the path for a path
	// group (path is non-nil), or the group ID for an LRF-only group.
	var complete []fetchedGroup
	incomplete := 0
	for _, g := range results {
		if !g.Complete() || len(g.Points) == 0 {
			incomplete++
			continue
		}
		fg := fetchedGroup{group: g, lrfCode: lrfOnly[g.ID], path: paths[g.ID]}
		if fg.path != nil {
			fg.lrfCode = fg.path.LRFCode
		}
		complete = append(complete, fg)
	}

	// Analyse each unique location once, even when several paths share it.
	locs := analyseLocations(complete, bt.config.Ducting)

	// One weather event per group, then the ducting events chosen by
	// ducting.publish. Both go out in one call.
	events := make([]beat.Event, 0, len(complete)+len(locs))
	for _, fg := range complete {
		events = append(events, weatherEvent(fg.group, fg.path, fg.lrfCode, snap.RunID, start, locs))
	}

	// Count the number of weather events before adding ducting events.
	weatherCount := len(events)

	// Fetch the ducting events for the analysed locations.
	ductEvents, notable, insufficient := ductingEvents(locs, bt.config.Ducting.Publish, snap.RunID, start)
	events = append(events, ductEvents...)

	// Mark simulated data so it can be told apart from real weather.
	if bt.mock != nil {
		tagMock(events)
	}

	// Publish all events, both weather and ducting.
	bt.client.PublishAll(events)

	// The request count is unique locations divided by batch size,
	// rounded up.
	bt.logger.Infof("Weather poll: %d groups (%d co-channel paths, %d LRF-only); points: %d requested, %d unique after merge, %d requests; %d weather events published, %d incomplete; %d ducting events published (publish: %s; %d locations, %d notable, %d insufficient data) (%s)",
		len(groups), len(snap.Paths), len(lrfOnly), len(coords), unique, (unique+batch-1)/batch, weatherCount, incomplete,
		len(ductEvents), bt.config.Ducting.Publish, len(locs), notable, insufficient, time.Since(start).Round(time.Millisecond))

	if fetchErr != nil {
		return fmt.Errorf("weather poll: %d of %d groups incomplete: %w", incomplete, len(groups), fetchErr)
	}
	return nil
}

// fetchedGroup is a weather group whose points all have weather, with the
// path and LRF it belongs to. path is nil for an LRF-only group.
type fetchedGroup struct {
	// group is the weather for the group's points (LRF, midpoint and
	// transmitter for a path; just the LRF for an LRF-only group). Its ID
	// is the path ID, or the LRF code for an LRF-only group.
	group openmeteo.GroupProfile
	// path is the co-channel path the group was built from, or nil.
	path *cochannel.Path
	// lrfCode is the LRF the group belongs to, upper-cased.
	lrfCode string
}

// analyseLocations runs the ducting analysis once per unique weather
// location in groups, keyed by profile ID, and records which paths and LRFs
// used each location.
func analyseLocations(groups []fetchedGroup, cfg ducting.Config) map[string]*location {
	locs := map[string]*location{}
	for _, fg := range groups {
		for _, pt := range fg.group.Points {
			// Points that shared one weather fetch have the same profile ID:
			// an LRF used by several paths, or nearby points merged by
			// coordinate_precision. Only the first one seen is analysed.
			id := profileID(pt.Profile)
			loc := locs[id]
			if loc == nil {
				// The location keeps the requested (possibly rounded)
				// coordinate, which is what its weather describes, not the
				// point's own coordinate.
				loc = &location{
					id:       id,
					coord:    pt.Profile.Requested,
					profile:  pt.Profile,
					analysis: ducting.Analyse(*pt.Profile, cfg),
					pathIDs:  map[string]struct{}{},
					lrfCodes: map[string]struct{}{},
				}
				locs[id] = loc
			}
			// Every group that uses the location adds its path and LRF, so
			// the ducting event lists all of them. Sets avoid duplicates
			// when a path's points share a location. An LRF-only group has
			// no path, so it adds only its LRF code.
			if fg.path != nil {
				loc.pathIDs[fg.group.ID] = struct{}{}
			}
			if fg.lrfCode != "" {
				loc.lrfCodes[fg.lrfCode] = struct{}{}
			}
		}
	}
	return locs
}

// ductingEvents builds the ducting events for locs, in profile ID order.
// With publish set to ducting.PublishNotable, only super-refractive and
// ducting locations get an event. notable and insufficient count every
// location, whether published or not.
func ductingEvents(locs map[string]*location, publish, runID string, at time.Time) (events []beat.Event, notable, insufficient int) {
	// Go maps iterate in random order; sort the IDs so events are built in
	// the same order on every poll.
	ids := make([]string, 0, len(locs))
	for id := range locs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		loc := locs[id]
		a := loc.analysis
		// Count before filtering, so the poll log shows how many events
		// "all" would publish and how many "notable" would keep.
		if a.Notable() {
			notable++
		}
		if !a.Usable() {
			insufficient++
		}
		// With "notable", normal, sub-refractive and insufficient_data
		// locations get no ducting event. The weather points at normal and
		// sub-refractive locations still carry the ducting summary.
		if publish == ducting.PublishNotable && !a.Notable() {
			continue
		}
		events = append(events, ductingEvent(loc, runID, at))
	}
	return events, notable, insufficient
}
