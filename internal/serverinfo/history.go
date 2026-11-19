package serverinfo

import (
	"sort"
	"time"

	"github.com/MowMdown/ReCON/internal/store"
)

// Point is the number of players from At until the next point.
type Point struct {
	At      int64 `json:"t"` // Unix seconds
	Players int   `json:"n"`
}

// Ranges are the population graph's choices.
var Ranges = map[string]time.Duration{"12h": 12 * time.Hour, "1d": 24 * time.Hour, "1w": 7 * 24 * time.Hour, "1m": 30 * 24 * time.Hour}

// maxPoints is about how many steps a graph gets; longer ranges are put
// in buckets showing each bucket's peak, so a short spike still shows.
const maxPoints = 400

// Population is the player count over since..now, as steps, from the
// sessions: exact for a short range, the peak of each bucket for a long one.
func Population(spans []store.Span, since, now time.Time) []Point {
	type ev struct {
		at time.Time
		d  int
	}
	evs := make([]ev, 0, 2*len(spans))
	n := 0
	for _, s := range spans {
		if !s.Start.After(since) {
			if s.End.After(since) {
				n++ // already on at the start
				evs = append(evs, ev{s.End, -1})
			}
			continue
		}
		evs = append(evs, ev{s.Start, +1}, ev{s.End, -1})
	}
	// At equal times leaves go first, so a reconnect doesn't count twice.
	sort.Slice(evs, func(a, b int) bool {
		if !evs[a].at.Equal(evs[b].at) {
			return evs[a].at.Before(evs[b].at)
		}
		return evs[a].d < evs[b].d
	})
	steps := []Point{{since.Unix(), n}}
	for _, e := range evs {
		if e.at.After(now) {
			break
		}
		n += e.d
		if n < 0 {
			n = 0
		}
		t := e.at.Unix()
		if last := &steps[len(steps)-1]; last.At == t {
			last.Players = n
			// Back to what it was before this moment: no step after all.
			if len(steps) > 1 && steps[len(steps)-2].Players == n {
				steps = steps[:len(steps)-1]
			}
		} else if last.Players != n {
			steps = append(steps, Point{t, n})
		}
	}
	if len(steps) <= maxPoints {
		return steps
	}
	// Buckets: each shows the most players at any time in it.
	width := now.Sub(since) / maxPoints
	out := make([]Point, 0, maxPoints)
	i := 0
	cur := 0 // the count at the start of the bucket
	for b := 0; b < maxPoints; b++ {
		start := since.Add(time.Duration(b) * width)
		end := start.Add(width)
		peak := cur
		for i < len(steps) && steps[i].At < end.Unix() {
			cur = steps[i].Players
			if cur > peak {
				peak = cur
			}
			i++
		}
		if len(out) == 0 || out[len(out)-1].Players != peak {
			out = append(out, Point{start.Unix(), peak})
		}
	}
	return out
}

// Stats sums up since..now: the most players at once, the average (over
// time), different players and sessions.
type Stats struct {
	Peak     int     `json:"peak"`
	Average  float64 `json:"average"`
	Unique   int     `json:"unique"`
	Sessions int     `json:"sessions"`
}

func SumUp(spans []store.Span, since, now time.Time) Stats {
	var st Stats
	guids := map[string]bool{}
	var total time.Duration
	for _, s := range spans {
		start, end := s.Start, s.End
		if start.Before(since) {
			start = since
		}
		if end.After(now) {
			end = now
		}
		if !end.After(start) {
			continue
		}
		total += end.Sub(start)
		guids[s.GUID] = true
		st.Sessions++
	}
	st.Unique = len(guids)
	if d := now.Sub(since); d > 0 {
		st.Average = float64(total) / float64(d)
	}
	for _, p := range Population(spans, since, now) {
		if p.Players > st.Peak {
			st.Peak = p.Players
		}
	}
	return st
}

// Busiest is the average number of players for each weekday (0 Sunday) and
// hour, in loc, over since..now.
func Busiest(spans []store.Span, since, now time.Time, loc *time.Location) [7][24]float64 {
	var on [7][24]time.Duration // player time in each slot
	var span [7][24]time.Duration
	// How long each slot was covered by since..now.
	for t := since.In(loc).Truncate(time.Hour); t.Before(now); t = t.Add(time.Hour) {
		a, b := t, t.Add(time.Hour)
		if a.Before(since) {
			a = since
		}
		if b.After(now) {
			b = now
		}
		if b.After(a) {
			lt := a.In(loc)
			span[lt.Weekday()][lt.Hour()] += b.Sub(a)
		}
	}
	for _, s := range spans {
		start, end := s.Start, s.End
		if start.Before(since) {
			start = since
		}
		if end.After(now) {
			end = now
		}
		for t := start; t.Before(end); {
			lt := t.In(loc)
			// The start of t's local hour, by subtracting the minutes and
			// seconds: rebuilding it with time.Date picks the first of the
			// two 1:00s when clocks go back, which is before t, and the
			// loop would never end.
			next := t.Add(-time.Duration(lt.Minute())*time.Minute - time.Duration(lt.Second())*time.Second -
				time.Duration(lt.Nanosecond())).Add(time.Hour)
			if !next.After(t) {
				next = t.Add(time.Hour)
			}
			if next.After(end) {
				next = end
			}
			on[lt.Weekday()][lt.Hour()] += next.Sub(t)
			t = next
		}
	}
	var out [7][24]float64
	for d := 0; d < 7; d++ {
		for h := 0; h < 24; h++ {
			if span[d][h] > 0 {
				out[d][h] = float64(on[d][h]) / float64(span[d][h])
			}
		}
	}
	return out
}
