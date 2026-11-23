package livemap

import (
	"time"
)

// mergeVehicles puts the occupied vehicles a sync brought (mod 1.0.3 on)
// into the last full list, by id (a new one is added). It returns a new
// slice: the old one may be being written out to a page.
func mergeVehicles(all, crewed []Vehicle) []Vehicle {
	out := make([]Vehicle, len(all), len(all)+len(crewed))
	copy(out, all)
	at := make(map[int]int, len(out))
	for i, v := range out {
		at[v.ID] = i
	}
	for _, v := range crewed {
		if i, ok := at[v.ID]; ok {
			out[i] = v
		} else {
			at[v.ID] = len(out)
			out = append(out, v)
		}
	}
	return out
}

// OccupiedVehicles is the vehicles with someone in them (and any whose last
// player just got out), for the Live Map to move every second (sent false:
// the mod doesn't send them apart, as one before 1.0.3).
func OccupiedVehicles(now time.Time) (vs []Vehicle, sent bool) {
	state.Lock()
	defer state.Unlock()
	if !connected(now) || !state.crewSent || state.vehiclesAt.IsZero() {
		return []Vehicle{}, false
	}
	return state.crewLast, true
}
