package cochannel

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/thetherington/tropobeat/internal/fcc"
)

// site is a transmitter's place among its station's sites on one channel.
type site struct{ n, of int }

// siteKey identifies one transmitter site: a station's facility ID, its RF
// channel and the site's location. It matches how the FCC import keys its
// documents (facility ID plus location).
func siteKey(d fcc.Document) string {
	return fmt.Sprintf("%s|%d|%.6f,%.6f", d.FacilityID, d.RFChannel, d.Location.Lat, d.Location.Lon)
}

// numberSites numbers the transmitter sites of every station that has more
// than one on the same channel (a distributed transmission system, often a
// main DTV site plus DTS sites): 1 to N, strongest (highest ERP) first, then
// by FCC antenna ID and location so the order is stable. Numbering is per
// station, not per LRF, so a site has the same number on every path. Sites
// of single-site stations are numbered 1 of 1.
func numberSites(all []fcc.Document) map[string]site {
	type group struct{ docs []fcc.Document }
	groups := map[string]*group{}
	for _, d := range all {
		k := fmt.Sprintf("%s|%d", d.FacilityID, d.RFChannel)
		g := groups[k]
		if g == nil {
			g = &group{}
			groups[k] = g
		}
		// The same site can appear once only (the import keys documents by
		// facility ID and location).
		if !slices.ContainsFunc(g.docs, func(x fcc.Document) bool { return siteKey(x) == siteKey(d) }) {
			g.docs = append(g.docs, d)
		}
	}

	out := make(map[string]site, len(all))
	for _, g := range groups {
		slices.SortFunc(g.docs, func(a, b fcc.Document) int {
			return cmp.Or(
				cmp.Compare(erp(b), erp(a)), // strongest first
				cmp.Compare(a.AntennaID, b.AntennaID),
				cmp.Compare(a.Location.Lat, b.Location.Lat),
				cmp.Compare(a.Location.Lon, b.Location.Lon),
			)
		})
		for i, d := range g.docs {
			out[siteKey(d)] = site{n: i + 1, of: len(g.docs)}
		}
	}
	return out
}

func erp(d fcc.Document) float64 {
	if d.ERPkW == nil {
		return 0
	}
	return *d.ERPkW
}
