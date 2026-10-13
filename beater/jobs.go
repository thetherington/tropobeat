package beater

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/elastic/beats/v7/libbeat/beat"

	"github.com/thetherington/tropobeat/internal/cochannel"
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
	all, err = lrf.Load(ctx, bt.es, bt.config.LRFIndex)
	if err != nil {
		return nil, nil, fmt.Errorf("loading LRF sites from index %s: %w", bt.config.LRFIndex, err)
	}
	if len(all) == 0 {
		return nil, nil, fmt.Errorf("no LRF sites found in index %s; load them with `tropobeat insert lrfcoordinates`", bt.config.LRFIndex)
	}

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
	defer bt.startWeatherOnce()
	start := time.Now().UTC()
	runID := start.Format("20060102T150405Z")

	all, selected, err := bt.loadLRFs(ctx)
	if err != nil {
		return err
	}

	received, err := cochannel.LoadReceived(ctx, bt.es, bt.config.DemodIndex, bt.config.Cochannel.DemodWindow)
	if err != nil {
		return err
	}
	received = filterReceived(received, all, selected, len(bt.config.LRFSites) > 0)

	res, err := cochannel.Discover(ctx, bt.es, cochannel.Options{
		FCCIndex: bt.config.FCCIndex,
		Radius:   bt.config.Cochannel.Radius,
	}, selected, received)
	if err != nil {
		return err
	}

	bt.state.Store(&snapshot{RunID: runID, At: start, LRFs: selected, Paths: res.Paths})

	events := make([]beat.Event, len(res.Paths))
	for i, p := range res.Paths {
		events[i] = cochannelEvent(p, runID, start)
	}
	bt.client.PublishAll(events)

	lrfsWithChannels := map[string]struct{}{}
	for _, r := range received {
		lrfsWithChannels[r.LRFCode] = struct{}{}
	}
	bt.logger.Infof("Co-channel discovery %s: %d LRFs, %d with demod channels, %d channels searched, %d own-station transmitters excluded, %d co-channel paths published (%s)",
		runID, len(selected), len(lrfsWithChannels), res.Channels, res.Excluded, len(res.Paths), time.Since(start).Round(time.Millisecond))

	if len(res.UnknownLRFs) > 0 {
		bt.logger.Warnf("Co-channel discovery: demod LRF IDs not in %s (skipped): %s", bt.config.LRFIndex, strings.Join(res.UnknownLRFs, ", "))
	}
	for _, n := range res.TooFar {
		bt.logger.Warnf("Co-channel discovery: skipped %s RF %d: %s (tags %q)", n.LRFCode, n.RFChannel, n.Detail, n.Tags)
	}
	for _, n := range res.NoCallSign {
		bt.logger.Infof("Co-channel discovery: skipped %s RF %d: %s (tags %q)", n.LRFCode, n.RFChannel, n.Detail, n.Tags)
	}
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
	known := map[string]bool{}
	for _, l := range all {
		known[strings.ToUpper(l.Code)] = false
	}
	for _, l := range selected {
		known[strings.ToUpper(l.Code)] = true
	}

	out := received[:0:0]
	for _, r := range received {
		isSelected, isKnown := known[r.LRFCode]
		if isSelected || (!isKnown && !limited) {
			out = append(out, r)
		}
	}
	return out
}

// runWeather fetches the weather for every co-channel path point and every
// LRF without paths, and publishes one event per complete group.
func (bt *tropobeat) runWeather(ctx context.Context) error {
	snap := bt.state.Load()
	if snap == nil || len(snap.LRFs) == 0 {
		return errors.New("no LRF sites loaded yet")
	}

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

	start := time.Now()
	results, fetchErr := bt.weather.GroupProfiles(ctx, groups, start)

	events := make([]beat.Event, 0, len(results))
	incomplete := 0
	for _, g := range results {
		if !g.Complete() || len(g.Points) == 0 {
			incomplete++
			continue
		}
		code := lrfOnly[g.ID]
		p := paths[g.ID]
		if p != nil {
			code = p.LRFCode
		}
		events = append(events, weatherEvent(g, p, code, snap.RunID))
	}
	bt.client.PublishAll(events)

	bt.logger.Infof("Weather poll: %d groups (%d co-channel paths, %d LRF-only); points: %d requested, %d unique after merge, %d requests; %d events published, %d incomplete (%s)",
		len(groups), len(snap.Paths), len(lrfOnly), len(coords), unique, (unique+batch-1)/batch, len(events), incomplete, time.Since(start).Round(time.Millisecond))

	if fetchErr != nil {
		return fmt.Errorf("weather poll: %d of %d groups incomplete: %w", incomplete, len(groups), fetchErr)
	}
	return nil
}
