package livemap

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// The deerisle item list (from the live server, 2026-11-21), plus a non-vehicle.
var deerisle = []CatalogItem{
	{"Boat_01_Black", "Rubber Boat", "vehicle"}, {"Boat_01_Blue", "Rubber Boat", "vehicle"}, {"Boat_01_Camo", "Rubber Boat", "vehicle"}, {"Boat_01_Orange", "Rubber Boat", "vehicle"},
	{"CivilianSedan", "Olga 24", "vehicle"}, {"CivilianSedan_Black", "Olga 24", "vehicle"}, {"CivilianSedan_Wine", "Olga 24", "vehicle"},
	{"Hatchback_02", "Gunter 2", "vehicle"}, {"Hatchback_02_Black", "Gunter 2", "vehicle"}, {"Hatchback_02_Blue", "Gunter 2", "vehicle"},
	{"Motorbike_01_Blue", "Jana 50", "vehicle"}, {"Motorbike_01_Red", "Jana 50", "vehicle"}, {"Motorbike_01_Yellow", "Jana 50", "vehicle"},
	{"Motorbike_02_Blue", "Bitrak 682", "vehicle"}, {"Motorbike_02_Green", "Bitrak 682", "vehicle"}, {"Motorbike_02_Red", "Bitrak 682", "vehicle"}, {"Motorbike_02_Yellow", "Bitrak 682", "vehicle"},
	{"OffroadHatchback", "Ada 4x4", "vehicle"}, {"OffroadHatchback_Blue", "Ada 4x4", "vehicle"}, {"OffroadHatchback_White", "Ada 4x4", "vehicle"},
	{"Offroad_02", "M1025", "vehicle"}, {"Offroad_02_Beige", "M1025", "vehicle"},
	{"RFMosquito", "Mosquito MK III Helicopter", "vehicle"},
	{"Sedan_02", "Sarka 120", "vehicle"}, {"Sedan_02_Grey", "Sarka 120", "vehicle"}, {"Sedan_02_Red", "Sarka 120", "vehicle"},
	{"Truck_01_Covered", "M3S Covered", "vehicle"}, {"Truck_01_Covered_Blue", "M3S Covered", "vehicle"}, {"Truck_01_Covered_Orange", "M3S Covered", "vehicle"},
	{"jmc_Boat_STAG_Black", "STAG Inflatable Boat", "vehicle"}, {"jmc_Boat_STAG_Red", "STAG Inflatable Boat", "vehicle"}, {"jmc_Boat_STAG_White", "STAG Inflatable Boat", "vehicle"},
	{"jmc_atv_Aqua", "ATV", "vehicle"}, {"jmc_atv_Blue", "ATV", "vehicle"}, {"jmc_atv_Green", "ATV", "vehicle"}, {"jmc_atv_Grey", "ATV", "vehicle"}, {"jmc_atv_Pink", "ATV", "vehicle"}, {"jmc_atv_Purple", "ATV", "vehicle"}, {"jmc_atv_Red", "ATV", "vehicle"},
	{"jmc_atv_STAG_Black", "STAG ATV", "vehicle"}, {"jmc_atv_STAG_Red", "STAG ATV", "vehicle"}, {"jmc_atv_STAG_White", "STAG ATV", "vehicle"},
	{"AKM", "AKM", "weapon"},
}

func keys[V any](m map[string]V) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestGenerateVehicleFileDeerisle(t *testing.T) {
	f := GenerateVehicleFile(deerisle)
	want := map[string][]string{
		"car": {"ada", "gunter", "m1025", "olga", "sarka"}, "truck": {"m3s"},
		"boat": {"rubber", "stag"}, "vehicle": {"atv", "bitrak", "jana", "mosquito", "stag"},
	}
	if got := keys(f); !reflect.DeepEqual(got, []string{"boat", "car", "truck", "vehicle"}) {
		t.Fatalf("categories %v", got)
	}
	for cat, names := range want {
		if got := keys(f[cat]); !reflect.DeepEqual(got, names) {
			t.Errorf("%s: %v, want %v", cat, got, names)
		}
	}
	checks := []struct {
		got, want map[string]string
	}{
		{f["car"]["olga"], map[string]string{"default": "CivilianSedan", "black": "CivilianSedan_Black", "wine": "CivilianSedan_Wine"}},
		{f["boat"]["rubber"], map[string]string{"black": "Boat_01_Black", "blue": "Boat_01_Blue", "camo": "Boat_01_Camo", "orange": "Boat_01_Orange"}},
		{f["vehicle"]["stag"], map[string]string{"black": "jmc_atv_STAG_Black", "red": "jmc_atv_STAG_Red", "white": "jmc_atv_STAG_White"}},
		{f["vehicle"]["mosquito"], map[string]string{"default": "RFMosquito"}},
	}
	for _, c := range checks {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("got %v, want %v", c.got, c.want)
		}
	}
	if f["vehicle"]["atv"]["pink"] != "jmc_atv_Pink" || len(f["vehicle"]["atv"]) != 7 {
		t.Errorf("atv %v", f["vehicle"]["atv"])
	}
	if f["truck"]["m3s"]["orange"] != "Truck_01_Covered_Orange" {
		t.Errorf("m3s %v", f["truck"]["m3s"])
	}
	if n := f.Count(); n != 13 {
		t.Errorf("Count %d, want 13", n)
	}
}

func TestGenerateVehicleFileNameClash(t *testing.T) {
	f := GenerateVehicleFile([]CatalogItem{{"jmc_atv_STAG_Red", "STAG ATV", "vehicle"}, {"jmc_quad_STAG", "STAG Quad", "vehicle"}})
	if got := keys(f["vehicle"]); !reflect.DeepEqual(got, []string{"stagatv", "stagquad"}) {
		t.Errorf("names %v", got)
	}
}

func writeFile(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadVehicleFileIgnoresCase(t *testing.T) {
	p := filepath.Join(t.TempDir(), "vehicles.json")
	writeFile(t, p, `{"Car": {"Olga": {"Black": "CivilianSedan_Black"}}}`)
	f, err := LoadVehicleFile(p, "deerisle")
	if err != nil {
		t.Fatal(err)
	}
	if f["car"]["olga"]["black"] != "CivilianSedan_Black" {
		t.Errorf("got %v", f)
	}
}

func TestLoadVehicleFileErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadVehicleFile(filepath.Join(dir, "none.json"), "deerisle"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}
	p := filepath.Join(dir, "vehicles.json")
	writeFile(t, p, "{\n  \"car\": {\n    \"olga\": \n  }\n}")
	_, err := LoadVehicleFile(p, "deerisle")
	if err == nil || !strings.HasPrefix(err.Error(), "vehicles.json for deerisle has an error at line 4:") {
		t.Errorf("bad JSON: %v", err)
	}
}

func TestCatalogClassIgnoresCase(t *testing.T) {
	setup(t)
	now := time.Now()
	r := syncReq()
	r.CatalogTotal, r.CatalogAt, r.Catalog = 1, 0, []CatalogItem{{"CivilianSedan_Black", "Olga 24", "vehicle"}}
	Sync(r, now)
	it, ok := CatalogClass("civiliansedan_black", now)
	if !ok || it.Type != "CivilianSedan_Black" {
		t.Errorf("got %+v %v", it, ok)
	}
	if _, ok := CatalogClass("Nope", now); ok {
		t.Error("found Nope")
	}
}

// syncCatalog syncs world with the whole item list at once.
func syncCatalog(world string, items []CatalogItem, now time.Time) {
	r := syncReq()
	r.World = world
	r.CatalogTotal, r.CatalogAt, r.Catalog = len(items), 0, items
	Sync(r, now)
}

func TestWritesVehicleFileOnce(t *testing.T) {
	setup(t)
	dataDir = t.TempDir()
	now := time.Now()
	syncCatalog("DeerIsle", deerisle, now)
	maybeWriteVehicleFile(now)
	p := filepath.Join(dataDir, "maps", "deerisle", "vehicles.json")
	f, err := LoadVehicleFile(p, "deerisle")
	if err != nil || f.Count() != 13 {
		t.Fatalf("written: %v %d", err, f.Count())
	}
	if data, _ := os.ReadFile(p); !strings.Contains(string(data), "\n  \"boat\": {") {
		t.Errorf("not 2-space indented:\n%s", data)
	}
	edited := `{"car":{"x":{"default":"CivilianSedan"}}}`
	writeFile(t, p, edited)
	maybeWriteVehicleFile(now)
	if data, _ := os.ReadFile(p); string(data) != edited {
		t.Errorf("rewrote the admin's file:\n%s", data)
	}
	// A reconnect on another map writes that map's, and leaves deerisle's.
	later := now.Add(time.Hour)
	syncCatalog("chernarusplus", deerisle, later)
	maybeWriteVehicleFile(later)
	if _, err := os.Stat(filepath.Join(dataDir, "maps", "chernarusplus", "vehicles.json")); err != nil {
		t.Errorf("chernarusplus: %v", err)
	}
	if data, _ := os.ReadFile(p); string(data) != edited {
		t.Error("deerisle's file changed")
	}
}

func TestNoVehicleFileWithoutMod(t *testing.T) {
	setup(t)
	dataDir = t.TempDir()
	now := time.Now()
	maybeWriteVehicleFile(now)
	if _, err := os.Stat(filepath.Join(dataDir, "maps")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("maps folder made without the mod: %v", err)
	}
	if _, _, err := VehicleChoices(now); err == nil || err.Error() != "recon-mod isn't running on this server" {
		t.Errorf("got %v", err)
	}
}

func TestVehicleChoicesAndUnlisted(t *testing.T) {
	setup(t)
	dataDir = t.TempDir()
	now := time.Now()
	r := syncReq()
	r.World = "deerisle"
	r.CatalogTotal, r.CatalogAt, r.Catalog = len(deerisle)+1, 0, deerisle
	Sync(r, now) // one short: the list isn't complete
	if _, _, err := VehicleChoices(now); err == nil || err.Error() != "the item list hasn't come in from the mod yet" {
		t.Errorf("incomplete: %v", err)
	}
	setup(t)
	dataDir = t.TempDir()
	syncCatalog("deerisle", deerisle, now)
	if _, _, err := VehicleChoices(now); err == nil || err.Error() != "there's no vehicles.json for deerisle yet" {
		t.Errorf("no file: %v", err)
	}
	maybeWriteVehicleFile(now)
	f, world, err := VehicleChoices(now)
	if err != nil || world != "deerisle" || f["car"]["olga"] == nil {
		t.Fatalf("got %v %q %v", f, world, err)
	}
	writeFile(t, filepath.Join(dataDir, "maps", "deerisle", "vehicles.json"), `{"car":{"olga":{"default":"civiliansedan"}}}`)
	un := UnlistedVehicles(now)
	if len(un) != 41 {
		t.Errorf("unlisted %d, want 41", len(un))
	}
	for _, it := range un {
		if it.Type == "CivilianSedan" {
			t.Error("CivilianSedan listed as missing")
		}
	}
}
