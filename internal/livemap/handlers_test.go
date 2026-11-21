package livemap

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"io/fs"
)

// The mod's JSON reader fills its classes from ReCON's reply, so every
// command field must be in it, even when empty.
func TestModSyncOverHTTP(t *testing.T) {
	setup(t)
	openTestDB(t)
	mux, quiet := startTest(t, "secret")
	post := func(path, body string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	sync := `{"v":1,"mod":"0.1.0","key":"%s","world":"chernarusplus","year":2026,"month":11,"day":20,"hour":14,"minute":5,
		"players":[{"steamId":"76561198000000001","name":"bob","x":7500.5,"y":10,"z":7600,"alive":true,"health":100,"unconscious":false}],
		"events":[],"results":[]}`
	if code, out := post("/api/mod/sync", strings.Replace(sync, "%s", "wrong", 1)); code != http.StatusForbidden || out["ok"] != false || out["error"] == "" {
		t.Fatalf("wrong key: %d %v", code, out)
	}
	if code, out := post("/api/mod/sync", strings.Replace(sync, "%s", "secret", 1)); code != 200 || out["ok"] != true {
		t.Fatalf("sync: %d %v", code, out)
	}
	guid := battlEyeGUID(76561198000000001)
	if code, out := post("/api/mod/players/"+guid+"/message", `{"text":"hi"}`); code != 200 {
		t.Fatalf("queue: %d %v", code, out)
	}
	if code, out := post("/api/mod/players/"+guid+"/fly", `{}`); code != http.StatusBadRequest {
		t.Fatalf("unknown action: %d %v", code, out)
	}
	_, out := post("/api/mod/sync", strings.Replace(sync, "%s", "secret", 1))
	cmds, _ := out["commands"].([]any)
	if len(cmds) != 1 {
		t.Fatalf("reply %v", out)
	}
	for _, k := range []string{"id", "action", "steamId", "target", "x", "y", "z", "stat", "value", "text", "title", "seconds"} {
		if _, ok := cmds[0].(map[string]any)[k]; !ok {
			t.Errorf("command has no %q: %v", k, cmds[0])
		}
	}
	for _, k := range []string{"ok", "error", "interval", "commands", "watch"} {
		if _, ok := out[k]; !ok {
			t.Errorf("reply has no %q", k)
		}
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mod/players/"+guid, nil))
	var one map[string]any
	json.Unmarshal(rec.Body.Bytes(), &one)
	if one["live"] != true || one["kills"] != float64(0) {
		t.Errorf("player: %v", one)
	}
	// Opening them asks the mod for their inventory; what it sends comes back.
	if _, out := post("/api/mod/sync", strings.Replace(sync, "%s", "secret", 1)); fmt.Sprint(out["watch"]) != "[76561198000000001]" {
		t.Fatalf("watch: %v", out["watch"])
	}
	withInv := strings.Replace(sync, `"unconscious":false`, `"unconscious":false,"inv":1,"inventory":[{"id":3,"type":"AKM","name":"AKM","slot":"Shoulder","row":-1,"col":-1,
		"attachments":[{"id":4,"type":"Mag_AKM_30Rnd","slot":"magazine","row":-1,"col":-1,"quantity":17,"quantityMax":30,"unit":"rounds","attachments":[],"cargo":[]}],"cargo":[]}]`, 1)
	if code, out := post("/api/mod/sync", strings.Replace(withInv, "%s", "secret", 1)); code != 200 {
		t.Fatalf("sync with inventory: %d %v", code, out)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mod/players/"+guid, nil))
	var withItems struct {
		Inventory Inventory `json:"inventory"`
	}
	json.Unmarshal(rec.Body.Bytes(), &withItems)
	if it := withItems.Inventory.Items; len(it) != 1 || it[0].Slot != "Shoulder" || it[0].Attachments[0].Quantity != 17 {
		t.Errorf("inventory: %s", rec.Body.String())
	}
	if !quiet(httptest.NewRequest(http.MethodPost, "/api/mod/sync", nil)) {
		t.Error("the mod's syncs would fill the log")
	}
}

// startTest starts the panel with a stand-in for ReCON and returns its
// routes and request-log filter.
func startTest(t *testing.T, password string) (*http.ServeMux, func(*http.Request) bool) {
	t.Helper()
	mux := http.NewServeMux()
	var quiet func(*http.Request) bool
	var nav []string
	err := Start(Host{
		DB:           db,
		RCONPassword: func() string { return password },
		AddRoute:     func(p string, h http.HandlerFunc) { mux.HandleFunc(p, h) },
		AddNavLink:   func(key, label, href string) { nav = append(nav, href) },
		AddQuiet:     func(f func(*http.Request) bool) { quiet = f },
		Page: func(files fs.FS, name string) http.HandlerFunc {
			if _, err := fs.Stat(files, name); err != nil {
				t.Errorf("page %s: %v", name, err)
			}
			return func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("page")) }
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(nav) != 1 || nav[0] != "/live" {
		t.Errorf("nav %v", nav)
	}
	t.Cleanup(func() { keySource = func() string { return "" } })
	return mux, quiet
}

// DayZ may write true/false as 1/0: both read. A sync ReCON can't read is
// answered with the reason.
func TestSyncFlagsAndBadJSON(t *testing.T) {
	setup(t)
	openTestDB(t)
	mux, _ := startTest(t, "secret")
	post := func(body string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/mod/sync", strings.NewReader(body)))
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	code, out := post(`{"v":1,"key":"secret","world":"enoch","weather":{"timeFrozen":1},
		"players":[{"steamId":"76561198000000001","name":"a","alive":1,"unconscious":0,"restrained":false}],
		"results":[{"id":1,"ok":1,"error":""}]}`)
	if code != 200 || out["ok"] != true {
		t.Fatalf("1/0 flags: %d %v", code, out)
	}
	if p, ok := PlayerOf(battlEyeGUID(76561198000000001), time.Now()); !ok || !bool(p.Alive) || bool(p.Unconscious) {
		t.Errorf("player %+v", p)
	}
	code, out = post(`{"v":1,"key":"secret","players":[{"alive":"maybe"}]}`)
	if code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "couldn't read") {
		t.Errorf("bad JSON: %d %v", code, out)
	}
}
