package livemap

// Event spots: every place the mission lets an event spawn (helicopter
// crashes, convoys, police, gas zones...), and which of them are up now.
//
// The mod sends the mission's cfgeventspawns.xml, db/events.xml
// and cfgeventgroups.xml when asked (one a sync). ReCON works out the spots
// from them and sends the mod the ones to check (spotChecks); the mod
// checks a slice of them each sync and says which have the event's objects
// near them. Gas zones come from the world events instead: a dynamic spot
// is up when a gas world event is near it, and a static zone (one on no
// spot) always shows.

import (
	"encoding/xml"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// MissionFile is a mission file the mod sent (ReCON_MissionFile).
type MissionFile struct {
	Name  string `json:"name"`
	Found Flag   `json:"found"`
	Text  string `json:"text"`
}

// MissionWrite is a bounded request for the mod to replace an existing mission XML file.
type MissionWrite struct {
	ID int `json:"id"`
	Name string `json:"name"`
	Text string `json:"text"`
}

// MissionWriteResult acknowledges a mission file write performed by the mod.
type MissionWriteResult struct {
	ID int `json:"id"`
	OK bool `json:"ok"`
	Error string `json:"error"`
}

// SpotCheck is a spot for the mod to check (ReCON_SpotCheck): up when an
// object of one of Types is within Radius of X, Z.
type SpotCheck struct {
	ID     int      `json:"id"`
	X      float64  `json:"x"`
	Z      float64  `json:"z"`
	Radius float64  `json:"radius"`
	Types  []string `json:"types"`
}

// SpotState is one checked spot (ReCON_SpotState).
type SpotState struct {
	ID int  `json:"id"`
	On Flag `json:"on"`
}

// EventSpot is one place an event can be, as the Live Map gets it.
type EventSpot struct {
	ID       int     `json:"id"`
	Event    string  `json:"event"`    // the mission's name: StaticHeliCrash
	Name     string  `json:"name"`     // Heli crash
	Category string  `json:"category"` // heli, convoy, police, medical, gas, other
	X        float64 `json:"x"`
	Z        float64 `json:"z"`
	On       bool    `json:"on"`
	Checked  bool    `json:"checked"`          // the mod has looked at it (it may still be off)
	Radius   float64 `json:"radius,omitempty"` // a gas zone's, while up
	Static   bool    `json:"static,omitempty"` // a gas zone that is always there (ContaminatedArea_Static)
	Hot      bool    `json:"hot,omitempty"`    // a hot (not toxic) zone: Sakhal's geysers
}

// missionFiles are the files asked for, in order.
var missionFiles = []string{"db/events.xml", "cfgeventspawns.xml", "cfgeventgroups.xml"}

const (
	spotRadius      = 30.0  // how near a single-object event's objects must be
	groupSpotRadius = 45.0  // and a group event's (a convoy is long)
	gasNear         = 150.0 // a gas world event this near a dynamic spot puts it up
	crashNear       = 60.0  // a crash world event this near a heli spot puts it up
	askAgain        = 10 * time.Second
)

var spots struct {
	sync.Mutex
	world  string            // the world the files are for
	files  map[string]string // name -> text ("" when the mission has none)
	asked  map[string]time.Time
	list   []EventSpot // built once all files are in
	checks []SpotCheck
	built  bool
	on     map[int]bool
	seen   map[int]bool
}

// resetSpots forgets everything (a new connection: the mission may differ).
func resetSpots() {
	spots.Lock()
	defer spots.Unlock()
	spots.world, spots.files, spots.asked, spots.list, spots.checks, spots.built = "", nil, nil, nil, nil, false
	spots.on, spots.seen = nil, nil
}

// takeSpots takes in a sync's mission files and spot states and says what
// to ask the mod next: the files still missing (one at a time) and the
// spots to check, when the mod holds another number of them.
func takeSpots(req SyncRequest, now time.Time) (want []string, set bool, checks []SpotCheck) {
	if req.SpotsHeld == nil {
		return nil, false, nil
	}
	world := strings.ToLower(req.World)
	spots.Lock()
	defer spots.Unlock()
	if world != spots.world {
		spots.world, spots.files, spots.asked, spots.list, spots.checks, spots.built = world, map[string]string{}, map[string]time.Time{}, nil, nil, false
		spots.on, spots.seen = map[int]bool{}, map[int]bool{}
	}
	for _, f := range req.MissionFiles {
		if _, asked := spots.asked[f.Name]; !asked {
			continue
		}
		text := ""
		if f.Found {
			text = f.Text
		}
		spots.files[f.Name] = text
	}
	if !spots.built {
		for _, name := range missionFiles {
			if _, ok := spots.files[name]; ok {
				continue
			}
			if at, ok := spots.asked[name]; !ok || now.Sub(at) > askAgain {
				spots.asked[name] = now
				want = []string{name}
			}
			return want, false, nil
		}
		spots.list, spots.checks = buildSpots(spots.files["db/events.xml"], spots.files["cfgeventspawns.xml"], spots.files["cfgeventgroups.xml"])
		spots.built = true
	}
	for _, st := range req.Spots {
		spots.on[st.ID] = bool(st.On)
		spots.seen[st.ID] = true
	}
	if *req.SpotsHeld != len(spots.checks) {
		return nil, true, spots.checks
	}
	return nil, false, nil
}

// EventSpots is every spot with whether it is up, the gas zones on no spot,
// and crashes on no spot (sent false: the files haven't come in yet).
func EventSpots(now time.Time) (list []EventSpot, sent bool) {
	events, evSent := WorldEvents(now)
	spots.Lock()
	built := spots.built
	if built {
		list = make([]EventSpot, len(spots.list))
		copy(list, spots.list)
		for i := range list {
			list[i].On = spots.on[list[i].ID]
			list[i].Checked = spots.seen[list[i].ID]
		}
	}
	world := spots.world
	spots.Unlock()
	if !built || !isConnected(now) {
		return []EventSpot{}, false
	}
	if !evSent {
		events = nil
	}
	used := make([]bool, len(events))
	for i := range list {
		s := &list[i]
		switch s.Category {
		case "gas":
			s.Checked = evSent
			for j, e := range events {
				if e.Kind == "crash" || used[j] {
					continue
				}
				if math.Hypot(e.X-s.X, e.Z-s.Z) <= gasNear {
					s.On, s.Radius, used[j] = true, e.Radius, true
					break
				}
			}
		case "heli":
			for j, e := range events {
				if e.Kind == "crash" && !used[j] && math.Hypot(e.X-s.X, e.Z-s.Z) <= crashNear {
					s.On, s.Checked, used[j] = true, true, true
					break
				}
			}
		}
	}
	// A gas spot's zone's size is kept while it is up; once it is down the
	// spot shows that last size (none: never seen up, no circle).
	for i := range list {
		s := &list[i]
		if s.Category != "gas" {
			continue
		}
		if s.On {
			keepGasRadius(world, s.X, s.Z, s.Radius)
		} else if r, ok := lastGasRadius(world, s.X, s.Z); ok {
			s.Radius = r
		}
	}
	next := 100000
	for j, e := range events {
		if used[j] {
			continue
		}
		next++
		switch e.Kind {
		case "crash":
			list = append(list, EventSpot{ID: next, Event: e.Type, Name: "Heli crash", Category: "heli", X: e.X, Z: e.Z, On: true, Checked: true})
		default:
			hot := e.Kind == "area"
			name := "Gas zone"
			if hot {
				name = "Hot zone"
			}
			static := strings.Contains(strings.ToLower(e.Type), "static")
			list = append(list, EventSpot{ID: next, Event: e.Type, Name: name, Category: "gas", X: e.X, Z: e.Z, On: true, Checked: true, Radius: e.Radius, Static: static, Hot: hot})
		}
	}
	return list, true
}

// handleModEventSpots serves GET /api/mod/eventspots.
func handleModEventSpots(w http.ResponseWriter, r *http.Request) {
	list, sent := EventSpots(time.Now())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "spots": list, "sent": sent})
}

// ---- the mission's files --------------------------------------------------

type xmlEvents struct {
	Events []struct {
		Name     string `xml:"name,attr"`
		Active   string `xml:"active"`
		Children struct {
			Child []struct {
				Type string `xml:"type,attr"`
			} `xml:"child"`
		} `xml:"children"`
	} `xml:"event"`
}

type xmlSpawns struct {
	Events []struct {
		Name string `xml:"name,attr"`
		Pos  []struct {
			X     string `xml:"x,attr"`
			Z     string `xml:"z,attr"`
			Group string `xml:"group,attr"`
		} `xml:"pos"`
	} `xml:"event"`
}

type xmlGroups struct {
	Groups []struct {
		Name  string `xml:"name,attr"`
		Child []struct {
			Type string `xml:"type,attr"`
		} `xml:"child"`
	} `xml:"group"`
}

// parseXML reads a mission file leniently (they are hand edited).
func parseXML(text string, v any) {
	if strings.TrimSpace(text) == "" {
		return
	}
	d := xml.NewDecoder(strings.NewReader(text))
	d.Strict = false
	d.AutoClose = xml.HTMLAutoClose
	d.Entity = xml.HTMLEntity
	_ = d.Decode(v)
}

// spotCategory sorts an event by its name; "" leaves it out.
func spotCategory(name string) string {
	if !strings.HasPrefix(name, "Static") || strings.HasPrefix(name, "StaticPlant") {
		return ""
	}
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "helicrash"), strings.Contains(n, "santacrash"):
		return "heli"
	case strings.Contains(n, "convoy"):
		return "convoy"
	case strings.Contains(n, "police"):
		return "police"
	case strings.Contains(n, "ambulance"), strings.Contains(n, "medic"):
		return "medical"
	case strings.Contains(n, "contaminated"):
		return "gas"
	}
	return "other"
}

// spotNames are the names of the events the vanilla maps and Deer Isle
// have; any other is its name split into words ("StaticWaterTruck": Water
// Truck).
var spotNames = map[string]string{
	"StaticHeliCrash": "Heli crash", "StaticHeliCrashBig": "Heli crash (big)", "StaticSantaCrash": "Santa's sleigh",
	"StaticMilitaryConvoy": "Convoy", "StaticConvoys": "Convoy",
	"StaticPoliceCar": "Police car", "StaticPoliceSituation": "Police checkpoint", "StaticPoliceCheckpoints": "Police checkpoint",
	"StaticAmbulance": "Ambulance", "StaticVanAmbulance": "Ambulance",
	"StaticContaminatedArea": "Gas zone",
	"StaticTrain":            "Train wreck", "StaticWreckedTrains": "Train wreck",
	"StaticAirplaneCrate": "Airplane crates", "StaticBoatSubmarine": "Submarine",
	"StaticBoatFishing": "Fishing boat", "StaticBoatMilitary": "Patrol boat",
	"StaticContainerLocked": "Locked container", "StaticWaterTruck": "Water truck",
	"StaticSurvivorCamps": "Survivor camp", "StaticScientist": "Frozen scientist",
	"StaticVanPinkies": "Pinkies van", "StaticSkeletonEventspawn": "Skeletons",
	"StaticBonfire": "Bonfire", "StaticChristmasTree": "Christmas tree",
}

func spotName(event string) string {
	if n, ok := spotNames[event]; ok {
		return n
	}
	rest := strings.TrimPrefix(event, "Static")
	var b strings.Builder
	for i, r := range rest {
		if i > 0 && unicode.IsUpper(r) {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// buildSpots works out the spots from the three files: every Static event
// the server has on (events.xml active, or every one when there's no
// events.xml), at each of its positions, with the classes that make it.
func buildSpots(eventsXML, spawnsXML, groupsXML string) ([]EventSpot, []SpotCheck) {
	var ev xmlEvents
	var sp xmlSpawns
	var gr xmlGroups
	parseXML(eventsXML, &ev)
	parseXML(spawnsXML, &sp)
	parseXML(groupsXML, &gr)
	children := map[string][]string{}
	active := map[string]bool{}
	for _, e := range ev.Events {
		active[e.Name] = strings.TrimSpace(e.Active) == "1"
		for _, c := range e.Children.Child {
			if c.Type != "" {
				children[e.Name] = append(children[e.Name], c.Type)
			}
		}
	}
	groups := map[string][]string{}
	for _, g := range gr.Groups {
		for _, c := range g.Child {
			if c.Type != "" {
				groups[g.Name] = append(groups[g.Name], c.Type)
			}
		}
	}
	var list []EventSpot
	var checks []SpotCheck
	id := 0
	for _, e := range sp.Events {
		cat := spotCategory(e.Name)
		if cat == "" || (len(ev.Events) > 0 && !active[e.Name]) {
			continue
		}
		for _, p := range e.Pos {
			x, errX := strconv.ParseFloat(strings.TrimSpace(p.X), 64)
			z, errZ := strconv.ParseFloat(strings.TrimSpace(p.Z), 64)
			if errX != nil || errZ != nil {
				continue
			}
			id++
			list = append(list, EventSpot{ID: id, Event: e.Name, Name: spotName(e.Name), Category: cat, X: x, Z: z})
			if cat == "gas" {
				continue // from the world events
			}
			types, radius := children[e.Name], spotRadius
			if p.Group != "" {
				types, radius = groups[p.Group], groupSpotRadius
			}
			types = uniqueTypes(types)
			if len(types) == 0 {
				continue
			}
			checks = append(checks, SpotCheck{ID: id, X: x, Z: z, Radius: radius, Types: types})
		}
	}
	return list, checks
}

func uniqueTypes(ts []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range ts {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}
