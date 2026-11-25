package livemap

// gasradius.go remembers, in memory, how big each dynamic gas zone was the
// last time it was up, by map and spot (a new mod connection doesn't forget
// it; a ReCON restart does). The mod only sends a zone's radius while it is
// up; while it is down the Live Map draws that last size grayed out, and no
// circle at all for a spot not seen up yet.

import (
	"fmt"
	"math"
	"sync"
)

var gasRadii = struct {
	sync.Mutex
	m map[string]float64 // gasKey -> radius (m)
}{m: map[string]float64{}}

// gasKey names a spot: its map and where it is, to the meter.
func gasKey(world string, x, z float64) string {
	return fmt.Sprintf("%s|%d|%d", world, int(math.Round(x)), int(math.Round(z)))
}

// lastGasRadius is the radius a spot's zone had when last up (ok false:
// not seen up yet).
func lastGasRadius(world string, x, z float64) (float64, bool) {
	gasRadii.Lock()
	defer gasRadii.Unlock()
	r, ok := gasRadii.m[gasKey(world, x, z)]
	return r, ok
}

// keepGasRadius remembers a spot's zone's radius while it is up.
func keepGasRadius(world string, x, z, r float64) {
	if r <= 0 {
		return
	}
	gasRadii.Lock()
	gasRadii.m[gasKey(world, x, z)] = r
	gasRadii.Unlock()
}
