package livemap

// vehiclefile.go is a map's vehicles.json: the vehicles the chat commands
// !car, !truck, !boat and !vehicle can spawn, by short name and color,
// which the admin edits. ReCON writes a first one from the mod's item list.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// VehicleFile is category (car, truck, boat, vehicle) -> short name ->
// color -> class. Every key is lowercase once loaded, so case never matters.
type VehicleFile map[string]map[string]map[string]string

// Count is how many vehicles (short names) the file has.
func (f VehicleFile) Count() int {
	n := 0
	for _, names := range f {
		n += len(names)
	}
	return n
}

// LoadVehicleFile reads path (world's file). A missing file's error wraps
// os.ErrNotExist; a file that doesn't parse says where.
func LoadVehicleFile(path, world string) (VehicleFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw map[string]map[string]map[string]string
	if err := json.Unmarshal(data, &raw); err != nil {
		var off int64 = -1
		var se *json.SyntaxError
		var te *json.UnmarshalTypeError
		if errors.As(err, &se) {
			off = se.Offset
		} else if errors.As(err, &te) {
			off = te.Offset
		}
		line := 1
		if off > 0 {
			line += bytes.Count(data[:min(int(off), len(data))], []byte("\n"))
		}
		return nil, fmt.Errorf("vehicles.json for %s has an error at line %d: %v", world, line, err)
	}
	f := VehicleFile{}
	for cat, names := range raw {
		c := strings.ToLower(cat)
		if f[c] == nil {
			f[c] = map[string]map[string]string{}
		}
		for name, colors := range names {
			n := strings.ToLower(name)
			if f[c][n] == nil {
				f[c][n] = map[string]string{}
			}
			for color, class := range colors {
				f[c][n][strings.ToLower(color)] = class
			}
		}
	}
	return f, nil
}

// vanillaCars are DayZ's own cars: the only classes the first file puts
// under car without a word in the name to go on.
var vanillaCars = map[string]bool{"civiliansedan": true, "sedan_02": true, "hatchback_02": true, "offroadhatchback": true, "offroad_02": true}

// GenerateVehicleFile makes the first vehicles.json from the item list's
// vehicles. Classes that share a display name are one vehicle in its
// colors; anything it can't place surely goes under vehicle, the catch-all.
func GenerateVehicleFile(items []CatalogItem) VehicleFile {
	groups := map[string][]string{}
	names := map[string]string{}
	for _, it := range items {
		if it.Kind != "vehicle" {
			continue
		}
		key := it.Name
		if key == "" || strings.HasPrefix(key, "$") {
			key = "class:" + it.Type // untranslated: alone, by class
		}
		groups[key] = append(groups[key], it.Type)
		names[key] = it.Name
	}

	type entry struct {
		words  []string
		take   int
		colors map[string]string
	}
	byCat := map[string][]*entry{}
	keysSorted := make([]string, 0, len(groups))
	for k := range groups {
		keysSorted = append(keysSorted, k)
	}
	sort.Strings(keysSorted)
	for _, k := range keysSorted {
		classes := groups[k]
		sort.Strings(classes)
		base := vehicleBase(classes)
		colors := map[string]string{}
		for _, c := range classes {
			col := "default"
			if len(classes) > 1 && c != base {
				col = strings.ToLower(strings.TrimPrefix(c, base+"_"))
			}
			colors[col] = c
		}
		words := []string{}
		if !strings.HasPrefix(k, "class:") {
			for _, w := range strings.Fields(names[k]) {
				if w = cleanName(w); w != "" {
					words = append(words, w)
				}
			}
		}
		if len(words) == 0 {
			words = []string{cleanName(base)}
		}
		byCat[vehicleCategory(base)] = append(byCat[vehicleCategory(base)], &entry{words: words, take: 1, colors: colors})
	}

	f := VehicleFile{}
	for cat, es := range byCat {
		// Two with one name both take their next word until they differ;
		// out of words, a number.
		name := func(e *entry) string { return strings.Join(e.words[:min(e.take, len(e.words))], "") }
		for {
			seen := map[string][]*entry{}
			for _, e := range es {
				seen[name(e)] = append(seen[name(e)], e)
			}
			grew := false
			for _, same := range seen {
				if len(same) < 2 {
					continue
				}
				for _, e := range same {
					if e.take < len(e.words) {
						e.take++
						grew = true
					}
				}
			}
			if !grew {
				break
			}
		}
		f[cat] = map[string]map[string]string{}
		for _, e := range es {
			n := name(e)
			for i := 2; f[cat][n] != nil; i++ {
				n = name(e) + fmt.Sprint(i)
			}
			f[cat][n] = e.colors
		}
	}
	return f
}

// vehicleBase is what a vehicle's classes share: their common prefix if
// it is one of them (CivilianSedan), else cut back to before its last _
// (Boat_01_Black, Boat_01_Blue -> Boat_01).
func vehicleBase(classes []string) string {
	p := classes[0]
	for _, c := range classes[1:] {
		for !strings.HasPrefix(c, p) {
			p = p[:len(p)-1]
		}
	}
	for _, c := range classes {
		if c == p {
			return p
		}
	}
	if i := strings.LastIndex(p, "_"); i > 0 {
		return p[:i]
	}
	return p
}

func vehicleCategory(base string) string {
	b := strings.ToLower(base)
	switch {
	case strings.Contains(b, "boat"):
		return "boat"
	case strings.Contains(b, "truck"):
		return "truck"
	case vanillaCars[b]:
		return "car"
	}
	return "vehicle"
}

// cleanName keeps a word's letters and digits, lowercased.
func cleanName(w string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(w) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// CatalogClass is the item list's entry for class, ignoring case (the
// file may spell it any way; the mod needs its exact spelling).
func CatalogClass(class string, now time.Time) (CatalogItem, bool) {
	state.Lock()
	defer state.Unlock()
	if !connected(now) || state.catalogTypes == nil {
		return CatalogItem{}, false
	}
	if it, ok := state.catalogTypes[class]; ok {
		return it, true
	}
	for t, it := range state.catalogTypes {
		if strings.EqualFold(t, class) {
			return it, true
		}
	}
	return CatalogItem{}, false
}

// dataDir is ReCON's data folder (Start sets it); each map's file is in
// its maps folder.
var dataDir string

func vehicleFilePath(world string) string {
	return filepath.Join(dataDir, "maps", strings.ToLower(world), "vehicles.json")
}

// maybeWriteVehicleFile writes the map's first vehicles.json once the mod
// is connected and its item list is in, if the map has none. It looks once
// a connection and map; an existing file is never touched, and without the
// mod nothing is made at all.
func maybeWriteVehicleFile(now time.Time) {
	if dataDir == "" {
		return
	}
	st, _ := Current(now)
	world := strings.ToLower(st.World)
	items, complete := Catalog(now)
	if !st.Connected || !complete || world == "" {
		return
	}
	state.Lock()
	done := state.vehFileFor == world
	state.vehFileFor = world
	state.Unlock()
	if done {
		return
	}
	p := vehicleFilePath(world)
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		return
	}
	f := GenerateVehicleFile(items)
	data, err := json.MarshalIndent(f, "", "  ")
	if err == nil {
		err = os.MkdirAll(filepath.Dir(p), 0o755)
	}
	if err == nil {
		tmp := p + ".tmp"
		if err = os.WriteFile(tmp, append(data, '\n'), 0o644); err == nil {
			err = os.Rename(tmp, p)
		}
	}
	if err != nil {
		log.Printf("recon-mod: writing vehicles.json for %s: %v", world, err)
		return
	}
	log.Printf("recon-mod: vehicles.json for %s written: %d vehicles", world, f.Count())
}

// VehicleChoices is the current map's vehicles.json and the map's name.
// Its errors are said to the player as they are.
func VehicleChoices(now time.Time) (VehicleFile, string, error) {
	st, _ := Current(now)
	if !st.Connected {
		return nil, "", errors.New("recon-mod isn't running on this server")
	}
	if _, complete := Catalog(now); !complete {
		return nil, "", errors.New("the item list hasn't come in from the mod yet")
	}
	world := strings.ToLower(st.World)
	if dataDir == "" {
		return nil, world, fmt.Errorf("there's no vehicles.json for %s yet", world)
	}
	f, err := LoadVehicleFile(vehicleFilePath(world), world)
	if errors.Is(err, os.ErrNotExist) {
		return nil, world, fmt.Errorf("there's no vehicles.json for %s yet", world)
	}
	return f, world, err
}

// UnlistedVehicles is the item list's vehicles that the current map's
// vehicles.json doesn't name anywhere (none without a file or the mod).
func UnlistedVehicles(now time.Time) []CatalogItem {
	f, _, err := VehicleChoices(now)
	if err != nil {
		return []CatalogItem{}
	}
	listed := map[string]bool{}
	for _, names := range f {
		for _, colors := range names {
			for _, class := range colors {
				listed[strings.ToLower(class)] = true
			}
		}
	}
	items, _ := Catalog(now)
	out := []CatalogItem{}
	for _, it := range items {
		if it.Kind == "vehicle" && !listed[strings.ToLower(it.Type)] {
			out = append(out, it)
		}
	}
	return out
}
