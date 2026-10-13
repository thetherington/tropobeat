package openmeteo

import (
	"context"
	"time"
)

// Group is a set of labelled points whose weather belongs together, such as
// an LRF, a co-channel transmitter and the midpoint of the path between them.
type Group struct {
	// ID identifies the group to the caller, e.g. "ABL:12345" for an
	// LRF/transmitter pair. It is returned unchanged.
	ID string
	// Points are the group's locations, in the order the caller wants them
	// back (e.g. LRF, midpoint, transmitter).
	Points []Point
}

// Point is a labelled location within a Group.
type Point struct {
	// Label names the point's role within the group, e.g. "lrf",
	// "midpoint" or "transmitter". It is returned unchanged.
	Label      string
	Coordinate Coordinate
}

// GroupProfile is the weather for one Group, in the same order as its Points.
type GroupProfile struct {
	ID     string
	Points []PointProfile
}

// PointProfile is the weather for one Point. Profile is nil if the weather
// for that point could not be fetched; the reason is reported in the error
// returned by GroupProfiles.
type PointProfile struct {
	Label      string
	Coordinate Coordinate
	Profile    *Profile
}

// Complete reports whether every point in the group has a profile.
func (g GroupProfile) Complete() bool {
	for _, p := range g.Points {
		if p.Profile == nil {
			return false
		}
	}
	return true
}

func (c *client) GroupProfiles(ctx context.Context, groups []Group, at time.Time) ([]GroupProfile, error) {
	var coords []Coordinate
	for _, g := range groups {
		for _, p := range g.Points {
			coords = append(coords, p.Coordinate)
		}
	}

	// Profiles removes duplicates, so points shared between groups (an LRF
	// with several transmitters, or a transmitter near several LRFs) are
	// only fetched once. With CoordinatePrecision set, points are rounded
	// first, and each point is matched to its profile by the same rounding.
	profiles, err := c.Profiles(ctx, coords, at)

	byCoord := make(map[Coordinate]*Profile, len(profiles))
	for i := range profiles {
		byCoord[profiles[i].Requested] = &profiles[i]
	}

	out := make([]GroupProfile, len(groups))
	for i, g := range groups {
		gp := GroupProfile{ID: g.ID, Points: make([]PointProfile, len(g.Points))}
		for j, p := range g.Points {
			gp.Points[j] = PointProfile{
				Label:      p.Label,
				Coordinate: p.Coordinate,
				Profile:    byCoord[Round(p.Coordinate, c.cfg.CoordinatePrecision)],
			}
		}
		out[i] = gp
	}

	return out, err
}
