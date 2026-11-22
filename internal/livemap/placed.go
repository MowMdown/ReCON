package livemap

// placed.go is the objects players placed (bases, tents, barrels...), as
// recon-mod 1.0.1 sends them every 30 s, and what is in one when the Live
// Map asks (like a vehicle's Info).

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Placed is one placed object (ReCON_Placed).
type Placed struct {
	ID       int     `json:"id"`  // this server run's
	PID      string  `json:"pid"` // persistent ID: b1:b2:b3:b4
	Type     string  `json:"type"`
	Kind     string  `json:"kind"` // base, tent, storage, trap or other
	Name     string  `json:"name"`
	X        float64 `json:"x"`
	Z        float64 `json:"z"`
	Health   float64 `json:"health"`   // 0..1
	Placer   string  `json:"placer"`   // "" unknown (placed before mod 1.0.1)
	PlacerID string  `json:"placerId"` // SteamID64
	PlacedAt int64   `json:"placedAt"` // unix; 0 unknown
}

// PlacedDetail is what is in and on a placed object, and its damage zones.
type PlacedDetail struct {
	ID    int       `json:"id"`
	Zones []Zone    `json:"zones"`
	Items []Item    `json:"items"`
	At    time.Time `json:"at"`
}

var placedState struct {
	sync.Mutex
	list   []Placed // nil: never sent (a mod before 1.0.1)
	watch  map[int]time.Time
	detail map[int]PlacedDetail
}

// takePlaced keeps what a sync brought, and says which objects to send the
// contents of next.
func takePlaced(req SyncRequest, now time.Time) (watch []int) {
	placedState.Lock()
	defer placedState.Unlock()
	if req.PlacedSent {
		placedState.list = req.Placed
		if placedState.list == nil {
			placedState.list = []Placed{}
		}
	}
	for _, d := range req.PlacedDetails {
		if placedState.detail == nil {
			placedState.detail = map[int]PlacedDetail{}
		}
		if d.Zones == nil {
			d.Zones = []Zone{}
		}
		if d.Items == nil {
			d.Items = []Item{}
		}
		d.At = now
		placedState.detail[d.ID] = d
		delete(placedState.watch, d.ID) // asked once
	}
	for id, until := range placedState.watch {
		if now.After(until) {
			delete(placedState.watch, id)
			continue
		}
		watch = append(watch, id)
	}
	sort.Ints(watch)
	return watch
}

func resetPlaced() {
	placedState.Lock()
	placedState.list, placedState.watch, placedState.detail = nil, nil, nil
	placedState.Unlock()
}

// PlacedObjects is every placed object as last sent (sent false: none yet,
// or the mod isn't connected).
func PlacedObjects(now time.Time) ([]Placed, bool) {
	if st, _ := Current(now); !st.Connected {
		return []Placed{}, false
	}
	placedState.Lock()
	defer placedState.Unlock()
	if placedState.list == nil {
		return []Placed{}, false
	}
	return placedState.list, true
}

// AskPlaced has the mod send what is in that object, in the next sync.
func AskPlaced(id int, now time.Time) {
	placedState.Lock()
	defer placedState.Unlock()
	if placedState.watch == nil {
		placedState.watch = map[int]time.Time{}
	}
	placedState.watch[id] = now.Add(watchFor)
	delete(placedState.detail, id)
}

// PlacedOf is one placed object, and its contents once the mod sent them.
func PlacedOf(id int, now time.Time) (p Placed, d PlacedDetail, haveDetail, ok bool) {
	ps, sent := PlacedObjects(now)
	if !sent {
		return
	}
	for _, x := range ps {
		if x.ID == id {
			p, ok = x, true
			break
		}
	}
	placedState.Lock()
	d, haveDetail = placedState.detail[id]
	placedState.Unlock()
	return
}

// PlacedDetails is the contents of each of those objects the mod has sent
// (left out: not yet). ask first has the mod send them again, as AskPlaced.
func PlacedDetails(ids []int, ask bool, now time.Time) map[int]PlacedDetail {
	if ask {
		for _, id := range ids {
			AskPlaced(id, now)
		}
	}
	placedState.Lock()
	defer placedState.Unlock()
	out := map[int]PlacedDetail{}
	for _, id := range ids {
		if d, ok := placedState.detail[id]; ok {
			out[id] = d
		}
	}
	return out
}

// maxDetailIDs is how many objects one /api/mod/placed/details asks about.
const maxDetailIDs = 200

// handleModPlacedDetails serves GET /api/mod/placed/details?ids=1,2,3
// (&ask=1: have the mod send them): the contents of several objects at
// once, for a group of them.
func handleModPlacedDetails(w http.ResponseWriter, r *http.Request) {
	var ids []int
	for _, s := range strings.Split(r.URL.Query().Get("ids"), ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		id, err := strconv.Atoi(s)
		if err != nil {
			fail(w, http.StatusBadRequest, "ids is a list of numbers")
			return
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 || len(ids) > maxDetailIDs {
		fail(w, http.StatusBadRequest, "ask about 1 to "+strconv.Itoa(maxDetailIDs)+" objects")
		return
	}
	ds := PlacedDetails(ids, r.URL.Query().Get("ask") == "1", time.Now())
	out := make(map[string]PlacedDetail, len(ds))
	for id, d := range ds {
		out[strconv.Itoa(id)] = d
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "details": out})
}

// handleModPlaced serves GET /api/mod/placed.
func handleModPlaced(w http.ResponseWriter, r *http.Request) {
	ps, sent := PlacedObjects(time.Now())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "placed": ps, "sent": sent})
}

// handleModPlacedOne serves GET /api/mod/placed/{id} (?ask=1: have the mod
// send what is in it).
func handleModPlacedOne(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	now := time.Now()
	if r.URL.Query().Get("ask") == "1" {
		AskPlaced(id, now)
	}
	p, d, have, ok := PlacedOf(id, now)
	if !ok {
		fail(w, http.StatusNotFound, "that isn't on the server any more")
		return
	}
	out := map[string]any{"ok": true, "placed": p}
	if have {
		out["detail"] = d
	}
	writeJSON(w, http.StatusOK, out)
}
