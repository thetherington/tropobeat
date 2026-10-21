package ducting

// State is the ducting state of a path or location, on a four-step scale
// for dashboards, maps and alerts. It combines the classes of the analyses
// with whether a duct traps the channel(s) concerned.
type State string

// Ducting states, in increasing order of interference risk.
const (
	// StateNormal: no point is super-refractive or ducting.
	StateNormal State = "normal"
	// StateSuperRefractive: the strongest point is super-refractive.
	StateSuperRefractive State = "super_refractive"
	// StateDucting: a point has a ducting layer, but no duct is thick enough
	// to trap the channel.
	StateDucting State = "ducting"
	// StateTrapping: a duct is thick enough to trap the channel.
	StateTrapping State = "trapping"
)

// Level returns the state as 0 (normal) to 3 (trapping), so dashboards can
// take the maximum over time or across paths.
func (s State) Level() int {
	switch s {
	case StateSuperRefractive:
		return 1
	case StateDucting:
		return 2
	case StateTrapping:
		return 3
	default:
		return 0
	}
}

// StateOf returns the ducting state of a set of analyses (a path's points,
// or one location): trapping if any duct traps any of mhz, otherwise the
// strongest class among the usable analyses. Sub-refractive counts as
// normal. ok is false when none is usable. With no frequencies, the state
// is never trapping.
func StateOf(as []Analysis, mhz []float64) (s State, ok bool) {
	s = StateNormal
	for _, a := range as {
		if !a.Usable() {
			continue
		}
		ok = true
		for _, d := range a.Ducts {
			for _, f := range mhz {
				if d.Traps(f) {
					return StateTrapping, true
				}
			}
		}
		switch {
		case a.Class == Ducting:
			s = StateDucting
		case a.Class == SuperRefractive && s == StateNormal:
			s = StateSuperRefractive
		}
	}
	return s, ok
}

// TV bands of US RF channels.
const (
	BandLowVHF  = "low_vhf"
	BandHighVHF = "high_vhf"
	BandUHF     = "uhf"
)

// Band returns the TV band of a US RF channel: low VHF (2–6), high VHF
// (7–13) or UHF (14–69). ok is false for any other channel.
func Band(rf int) (string, bool) {
	switch {
	case rf >= 2 && rf <= 6:
		return BandLowVHF, true
	case rf >= 7 && rf <= 13:
		return BandHighVHF, true
	case rf >= 14 && rf <= 69:
		return BandUHF, true
	default:
		return "", false
	}
}
