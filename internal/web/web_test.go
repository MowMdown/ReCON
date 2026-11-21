package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/flows"
	"github.com/MowMdown/ReCON/internal/game"
	"github.com/MowMdown/ReCON/internal/geoip"
	"github.com/MowMdown/ReCON/internal/serverinfo"
	"github.com/MowMdown/ReCON/internal/steam"
	"github.com/MowMdown/ReCON/internal/store"
	"github.com/MowMdown/ReCON/internal/test"
)

// testDB opens a fresh database for the pages and the packages they use,
// and resets the player tracker.
func testDB(t *testing.T) *store.Database {
	t.Helper()
	db := test.OpenDB(t)
	SetDatabase(db)
	flows.SetDatabase(db)
	game.SetDatabase(db)
	steam.SetDatabase(db)
	game.ResetTracker()
	return db
}

// resetWeb starts a test with no web session, and closes it and restores
// the RCON target after the test.
func resetWeb(t *testing.T) {
	oldAddr, oldPass := game.Target()
	game.CloseSession()
	t.Cleanup(func() {
		game.SetTarget(oldAddr, oldPass)
		game.CloseSession()
	})
}

// flowIDs lists the stored flows' numbers in display order.
func flowIDs(t *testing.T) []int64 {
	t.Helper()
	list, err := flows.List()
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{}
	for _, f := range list {
		ids = append(ids, f.ID)
	}
	return ids
}

func TestRootRedirectsToServerInfo(t *testing.T) {
	mux := buildWebRoutes()
	for path, want := range map[string]int{"/": http.StatusFound, "/plyers": http.StatusNotFound} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
		if want == http.StatusFound && rec.Header().Get("Location") != "/server" {
			t.Errorf("GET %s redirects to %q", path, rec.Header().Get("Location"))
		}
	}
}

func TestRconSettingsPage(t *testing.T) {
	testDB(t)
	resetWeb(t)
	f := test.NewServer(t)
	f.SetPassword("secret")
	host, port, _ := net.SplitHostPort(f.Addr())
	t.Setenv("RCON_HOST", "10.9.9.9")
	t.Setenv("RCON_PORT", "2306")
	t.Setenv("RCON_PASSWORD", "compose-password")
	mux := buildWebRoutes()
	call := func(method, body string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/api/rcon-settings", strings.NewReader(body))
		mux.ServeHTTP(rec, req)
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	// GET shows the defaults, the password included (shown as placeholder text).
	code, got := call(http.MethodGet, "")
	d, _ := got["defaults"].(map[string]any)
	if code != 200 || d["host"] != "10.9.9.9" || d["port"] != "2306" || d["password"] != "compose-password" {
		t.Fatalf("GET = %d %v", code, got)
	}

	// Bad input and a wrong password change nothing.
	if code, _ := call(http.MethodPost, `{"port": "70000"}`); code != http.StatusBadRequest {
		t.Fatalf("port 70000: %d", code)
	}
	if code, _ := call(http.MethodPost, `{"host": "`+host+`", "port": "`+port+`", "password": "wrong"}`); code != http.StatusBadGateway {
		t.Fatalf("wrong password: %d", code)
	}
	if s := game.CurrentOverrides(); s != (game.Settings{}) {
		t.Fatalf("kept after a failed connect: %+v", s)
	}

	// The right settings connect, are kept (in memory only), and become the target.
	if code, got := call(http.MethodPost, `{"host": " `+host+` ", "port": "`+port+`", "password": "secret"}`); code != 200 || got["ok"] != true {
		t.Fatalf("connect = %d %v", code, got)
	}
	if s := game.CurrentOverrides(); s.Host != host || s.Port != port || s.Password != "secret" {
		t.Fatalf("kept = %+v", s)
	}
	if addr, pass := game.Target(); addr != net.JoinHostPort(host, port) || pass != "secret" {
		t.Fatalf("target = %s %q", addr, pass)
	}
	if _, ok, _ := database.GetStoredSetting("rcon"); ok {
		t.Fatal("RCON Settings were written to the database")
	}

	// Empty fields use the defaults.
	if addr, pass := (game.Settings{Port: "1234"}).Effective(); addr != "10.9.9.9:1234" || pass != "compose-password" {
		t.Fatalf("defaults = %s %q", addr, pass)
	}
}

func TestStatusReportsNextRetry(t *testing.T) {
	resetWeb(t)
	oldNext, oldFails, oldPolled := game.NextPoll.Load(), game.Fails.Load(), game.Polled.Load()
	t.Cleanup(func() {
		game.NextPoll.Store(oldNext)
		game.Fails.Store(oldFails)
		game.Polled.Store(oldPolled)
	})
	game.Polled.Store(true)
	now := time.Now()

	// Down, with the next heartbeat 20s from now (see the watch's tests).
	game.Fails.Store(game.DownAfterMisses)
	game.NextPoll.Store(now.Add(20 * time.Second).UnixMilli())

	status := func() map[string]any {
		rec := httptest.NewRecorder()
		buildWebRoutes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	if ms, ok := status()["retryInMs"].(float64); !ok || ms < 19000 || ms > 20100 {
		t.Fatalf("retryInMs = %v while unavailable", status()["retryInMs"])
	}
	game.Fails.Store(0)
	if _, ok := status()["retryInMs"]; ok {
		t.Fatal("retryInMs sent while connected")
	}
}

func TestFlowAPIUsesMissingIDForNew(t *testing.T) {
	db := testDB(t)
	if err := db.EnsureFlowsTable(); err != nil {
		t.Fatal(err)
	}
	mux := buildWebRoutes()
	post := func(path, body string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	steps := `"trigger": "manual", "steps": [{"type": "message", "to": "all", "text": "hi"}]`
	if code, out := post("/api/flows", `{"name": "First", `+steps+`}`); code != 200 || out["id"] != float64(0) {
		t.Fatalf("create without id: %d %v", code, out)
	}
	if code, out := post("/api/flows", `{"id": null, "name": "Second", `+steps+`}`); code != 200 || out["id"] != float64(1) {
		t.Fatalf("create with id null: %d %v", code, out)
	}
	// id 0 updates flow #0 rather than creating another flow.
	if code, out := post("/api/flows", `{"id": 0, "name": "First (renamed)", `+steps+`}`); code != 200 || out["id"] != float64(0) {
		t.Fatalf("update #0: %d %v", code, out)
	}
	if got := fmt.Sprint(flowIDs(t)); got != "[0 1]" {
		t.Fatalf("flows: %s", got)
	}
	if f, _ := flows.Get(0); f.Name != "First (renamed)" {
		t.Fatalf("flow #0 = %q", f.Name)
	}
	// Flow 0 is reachable through the per-flow routes.
	if code, _ := post("/api/flows/0/delete", ""); code != 200 {
		t.Fatalf("delete #0: %d", code)
	}
	if code, _ := post("/api/flows", `{"id": -1, "name": "x", `+steps+`}`); code != http.StatusBadRequest {
		t.Fatalf("negative id: %d", code)
	}
}

// TestPageAPIKeys checks the JSON keys the pages read their lists from.
func TestPageAPIKeys(t *testing.T) {
	testDB(t)
	if err := flows.Init(); err != nil {
		t.Fatal(err)
	}
	if _, err := flows.Save(flows.Flow{Name: "MOTD", Trigger: "manual", Steps: []flows.Step{{Type: "message", To: "all", Text: "hi"}}}, true); err != nil {
		t.Fatal(err)
	}
	get := func(path string) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		buildWebRoutes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		return out
	}
	if list, ok := get("/api/flows")["flows"].([]any); !ok || len(list) != 1 {
		t.Fatalf("/api/flows has no flows list: %v", get("/api/flows"))
	}
	for _, key := range []string{"players", "offlinePlayers", "bans", "connected", "count"} {
		if _, ok := get("/api/players")[key]; !ok {
			t.Errorf("/api/players has no %q", key)
		}
	}
}

// TestFlowExportPicks checks that Export saves every flow, or with ?ids=
// only the picked ones, in list order.
func TestFlowExportPicks(t *testing.T) {
	testDB(t)
	if err := flows.Init(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"A", "B", "C"} {
		if _, err := flows.Save(flows.Flow{Name: name, Trigger: "manual", Steps: []flows.Step{{Type: "message", To: "all", Text: "hi"}}}, true); err != nil {
			t.Fatal(err)
		}
	}
	export := func(query string) (int, []string) {
		t.Helper()
		rec := httptest.NewRecorder()
		buildWebRoutes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/flows/export"+query, nil))
		var f flowFile
		json.Unmarshal(rec.Body.Bytes(), &f)
		var names []string
		for _, e := range f.Flows {
			names = append(names, e.Name)
		}
		return rec.Code, names
	}
	if code, names := export(""); code != 200 || strings.Join(names, ",") != "A,B,C" {
		t.Fatalf("export all = %d %v", code, names)
	}
	if code, names := export("?ids=2,0"); code != 200 || strings.Join(names, ",") != "A,C" {
		t.Fatalf("export ids=2,0 = %d %v, want A,C in list order", code, names)
	}
	if code, _ := export("?ids=x"); code != http.StatusBadRequest {
		t.Fatalf("bad ids: %d", code)
	}
}

// TestBanTransferRoutes checks the ban export and import routes: the
// export file holds only permanent bans (or the picked ones), and import
// rejects a file with no bans.
func TestBanTransferRoutes(t *testing.T) {
	db := testDB(t)
	resetWeb(t)
	game.SetTarget("127.0.0.1:1", "")
	if err := db.RecordPlayerBan(test.GUID1, "Cheater", "1.1.1.1", "aimbot", true, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordPlayerBan(test.GUID2, "Spammer", "2.2.2.2", "spam", false, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) (int, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		buildWebRoutes().ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		return rec.Code, rec.Body.String()
	}
	if code, body := call(http.MethodGet, "/api/bans/export", ""); code != 200 || !strings.Contains(body, test.GUID1) || strings.Contains(body, test.GUID2) {
		t.Fatalf("export = %d %s, want only the permanent ban", code, body)
	}
	if code, body := call(http.MethodGet, "/api/bans/export?guids="+test.GUID2, ""); code != 200 || strings.Contains(body, test.GUID1) {
		t.Fatalf("export of a timed-ban GUID = %d %s", code, body)
	}
	if code, _ := call(http.MethodGet, "/api/bans/export?guids=nope", ""); code != http.StatusBadRequest {
		t.Fatalf("bad guids: %d", code)
	}
	if code, _ := call(http.MethodPost, "/api/bans/import", `{"bans": []}`); code != http.StatusBadRequest {
		t.Fatalf("empty import: %d", code)
	}
}

// previewCrons are the schedules the preview can describe in words: the
// example flows' and a few common ones.
var previewCrons = []string{"*/20 * * * *", "0 */6 * * *", "0 0,6,12,18 * * *", "0 5 * * 1-5", "0 5/6 * * *", "0 6 * * *", "30 4 * * 1,4", "55 23,5,11,17 * * *"}

// TestPreview rebuilds docs/preview, the static preview pages, from the real
// templates, so they never drift from the app. It only runs when asked:
//
//	RECON_PREVIEW=1 go test -run TestPreview ./internal/web
//
// It writes players.html, bans.html and flows.html (each loading data.js and
// preview.js, the stand-in for the API), data.js (the chat commands, the
// example flows and the cron descriptions, from the Go code) and assets/ (the
// theme, as the app serves it).
func TestPreview(t *testing.T) {
	if os.Getenv("RECON_PREVIEW") != "1" {
		t.Skip("set RECON_PREVIEW=1 to rebuild docs/preview")
	}
	dir := filepath.Join("..", "..", "docs", "preview")
	pages := map[string]*template.Template{"players.html": playersPage, "bans.html": bansPage, "flows.html": flowsPage}
	for name, page := range pages {
		var b bytes.Buffer
		if err := page.Execute(&b, nil); err != nil {
			t.Fatal(err)
		}
		html := strings.Replace(b.String(), "<head>\n", "<head>\n<script src=\"data.js\"></script>\n<script src=\"preview.js\"></script>\n", 1)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(html), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(filepath.Join(dir, "assets")); err != nil {
		t.Fatal(err)
	}
	err := fs.WalkDir(assetFiles, "assets", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dir, path), 0o755)
		}
		data, err := assetFiles.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, path), data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}

	testDB(t)
	if err := flows.Init(); err != nil {
		t.Fatal(err)
	}
	crons := map[string]string{}
	for _, c := range previewCrons {
		rec := httptest.NewRecorder()
		buildWebRoutes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/flows/preview",
			strings.NewReader(`{"trigger": "schedule", "schedule": "`+c+`"}`)))
		var out struct{ Description string }
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Description == "" {
			t.Fatalf("describing %q: %s", c, rec.Body.String())
		}
		crons[c] = out.Description
	}
	examples, err := os.ReadFile(filepath.Join("..", "..", "examples", "flows.json"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(map[string]any{
		"commands": flows.ChatCommandDefs,
		"crons":    crons,
		"defaults": flows.DefaultChatSettings(),
		"examples": json.RawMessage(examples),
	}, "", "\t")
	if err != nil {
		t.Fatal(err)
	}
	js := "// Written by TestPreview (RECON_PREVIEW=1 go test -run TestPreview ./internal/web). Don't edit.\n" +
		"window.RECON_PREVIEW_DATA = " + string(data) + ";\n"
	if err := os.WriteFile(filepath.Join(dir, "data.js"), []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDeletePlayerRoute checks DELETE /api/players/{guid}: the name must
// match exactly, a player who is online can't be deleted, and once deleted
// the player is gone.
func TestDeletePlayerRoute(t *testing.T) {
	db := testDB(t)
	t0 := time.Now().Add(-time.Hour)
	if err := db.OpenPlayerSession(test.GUID1, "Marrow", "1.2.3.4", 3, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClosePlayerSession(test.GUID1, t0.Add(time.Minute), "disconnected"); err != nil {
		t.Fatal(err)
	}
	mux := buildWebRoutes()
	del := func(guid, body string) (int, string) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/players/"+guid, strings.NewReader(body)))
		return rec.Code, rec.Body.String()
	}

	if code, _ := del(test.GUID2, `{"name": "Marrow"}`); code != http.StatusNotFound {
		t.Errorf("unknown player: %d, want 404", code)
	}
	if code, _ := del(test.GUID1, `{"name": "marrow"}`); code != http.StatusBadRequest {
		t.Errorf("wrong name: %d, want 400", code)
	}
	if code, _ := del(test.GUID1, `{}`); code != http.StatusBadRequest {
		t.Errorf("no name: %d, want 400", code)
	}
	// Online: refused, nothing deleted.
	game.Track(battleye.ParseServerMessage("Player #3 Marrow (1.2.3.4:2304) connected", time.Now()))
	game.Track(battleye.ParseServerMessage("Player #3 Marrow - BE GUID: "+test.GUID1, time.Now()))
	if code, _ := del(test.GUID1, `{"name": "Marrow"}`); code != http.StatusConflict {
		t.Errorf("online player: %d, want 409", code)
	}
	game.ResetTracker()
	if code, body := del(test.GUID1, `{"name": "Marrow"}`); code != http.StatusOK {
		t.Fatalf("delete: %d %s", code, body)
	}
	if n := test.CountRows(t, db, "players"); n != 0 {
		t.Errorf("%d player row(s) left", n)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/players/"+test.GUID1+"/details", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("details after delete: %d, want 404", rec.Code)
	}
}

// TestDeveloperRoutes checks the Developer page's API: the list shows who is
// online and banned, and one player's data has every source, all their
// rows (nothing trimmed), the tracker entry and the flow variables.
func TestDeveloperRoutes(t *testing.T) {
	db := testDB(t)
	t.Cleanup(game.ResetTracker)
	t0 := time.Now().Add(-time.Hour)
	for i := 0; i < 3; i++ { // three past sessions
		at := t0.Add(time.Duration(i) * 10 * time.Minute)
		if err := db.OpenPlayerSession(test.GUID1, "Marrow", "1.2.3.4", 3, at); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ClosePlayerSession(test.GUID1, at.Add(time.Minute), "disconnected"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.RecordPlayerBan(test.GUID2, "Fenwick", "5.6.7.8", "cheating", false, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	game.Track(battleye.ParseServerMessage("Player #3 Marrow (1.2.3.4:2304) connected", time.Now()))
	game.Track(battleye.ParseServerMessage("Player #3 Marrow - BE GUID: "+test.GUID1, time.Now()))
	mux := buildWebRoutes()
	get := func(path string, out any) int {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if out != nil {
			if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
		}
		return rec.Code
	}

	if code := get("/developer", nil); code != http.StatusOK {
		t.Fatalf("/developer: %d", code)
	}
	var list struct {
		Players []struct {
			GUID     string `json:"guid"`
			OnlineID *int   `json:"onlineId"`
		} `json:"players"`
	}
	get("/api/developer/players", &list)
	if len(list.Players) != 1 || list.Players[0].GUID != test.GUID1 || list.Players[0].OnlineID == nil || *list.Players[0].OnlineID != 3 {
		t.Errorf("list: %+v, want Marrow online as #3", list.Players)
	}

	var one struct {
		Name      string `json:"name"`
		Variables []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
			Known bool   `json:"known"`
		} `json:"variables"`
		Sources []struct {
			Title string          `json:"title"`
			Data  json.RawMessage `json:"data"`
		} `json:"sources"`
	}
	if code := get("/api/developer/players/"+strings.ToUpper(test.GUID1), &one); code != http.StatusOK {
		t.Fatalf("player: %d", code)
	}
	src := map[string]json.RawMessage{}
	for _, s := range one.Sources {
		src[s.Title] = s.Data
	}
	var history []map[string]any
	json.Unmarshal(src["session_history"], &history)
	if one.Name != "Marrow" || len(history) != 3 {
		t.Errorf("name %q, %d history rows, want Marrow and 3", one.Name, len(history))
	}
	if !strings.Contains(string(src["Player tracker (game tracker)"]), `"id":3`) {
		t.Errorf("tracker: %s", src["Player tracker (game tracker)"])
	}
	vars := map[string]string{}
	for _, v := range one.Variables {
		if v.Known {
			vars[v.Name] = v.Value
		}
	}
	if vars["player_name"] != "Marrow" || vars["player_id"] != "3" || vars["player_role"] != "Player" {
		t.Errorf("variables: %v", vars)
	}

	// The timed ban held in memory shows up for the banned player.
	get("/api/developer/players/"+test.GUID2, &one)
	for _, s := range one.Sources {
		if s.Title == "Timed ban (store, memory only)" && !strings.Contains(string(s.Data), "cheating") {
			t.Errorf("timed ban: %s", s.Data)
		}
	}
	if code := get("/api/developer/players/nope", nil); code != http.StatusBadRequest {
		t.Errorf("bad GUID: %d, want 400", code)
	}
}

// TestOfflineListLastMessageNoBanned checks /api/players' offline list: a
// banned player is left out, and each player has the newest line they wrote
// in chat (not the messages sent to them).
func TestOfflineListLastMessageNoBanned(t *testing.T) {
	db := testDB(t)
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)
	for _, p := range []struct{ guid, name string }{{test.GUID1, "Marrow"}, {test.GUID2, "Fenwick"}} {
		if err := db.OpenPlayerSession(p.guid, p.name, "1.2.3.4", 3, t0); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ClosePlayerSession(p.guid, t0.Add(time.Minute), "disconnected"); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		at        time.Duration
		dir, line string
	}{
		{1, store.ChatIn, "(Global) Marrow: first"},
		{2, store.ChatIn, "(Side) Marrow: brb dinner"},
		{3, store.ChatOut, "> say 3 welcome back"},
	} {
		if err := db.AddChatMessage(t0.Add(c.at*time.Second), test.GUID1, c.dir, c.line); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.RecordPlayerBan(test.GUID2, "Fenwick", "1.2.3.4", "cheating", true, time.Time{}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	buildWebRoutes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/players", nil))
	var d struct {
		Offline []battleye.Player `json:"offlinePlayers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Offline) != 1 || d.Offline[0].GUID != test.GUID1 {
		t.Fatalf("offline list: %+v, want only Marrow (Fenwick is banned)", d.Offline)
	}
	p := d.Offline[0]
	if p.LastMessage != "brb dinner" || p.LastMessageChannel != "Side" || p.LastMessageUnix != t0.Add(2*time.Second).Unix() {
		t.Errorf("last message %q (%s, %d), want \"brb dinner\" (Side, %d)", p.LastMessage, p.LastMessageChannel, p.LastMessageUnix, t0.Add(2*time.Second).Unix())
	}
}

// TestServerInfoRoutes checks the Server Info API: the page and its data
// answer with nothing recorded yet, a mod update is acknowledged, and a bad
// range or unknown mod is refused.
func TestServerInfoRoutes(t *testing.T) {
	db := testDB(t)
	serverinfo.SetDatabase(db)
	t.Cleanup(func() { serverinfo.SetDatabase(nil) })
	if err := db.SaveModFromSteam(store.ModUpdate{WorkshopID: "1559212036", Name: "CF", SteamUpdated: 100}); err != nil {
		t.Fatal(err)
	}
	// Steam has a newer one.
	if _, err := db.Exec("UPDATE mod_updates SET steam_updated = 200"); err != nil {
		t.Fatal(err)
	}
	mux := buildWebRoutes()
	do := func(method, path string) int {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec.Code
	}
	for path, want := range map[string]int{
		"/server": 200, "/api/server": 200, "/api/server/population?range=7d": 200, "/api/server/population?range=30d": 200, "/api/server/population?range=12h": 200, "/api/server/population?range=1w": 400, "/api/server/population?range=all": 200,
		"/api/server/population?range=3y": 400, "/api/server/busiest?tz=America/New_York": 200, "/api/server/busiest?tz=nope": 200,
	} {
		if code := do(http.MethodGet, path); code != want {
			t.Errorf("GET %s = %d, want %d", path, code, want)
		}
	}
	if code := do(http.MethodPost, "/api/server/mods/1559212036/ack"); code != 200 {
		t.Errorf("ack = %d", code)
	}
	if rows, _ := db.ModUpdates(); rows["1559212036"].AckedUpdated != 200 {
		t.Errorf("not acknowledged: %+v", rows)
	}
	if code := do(http.MethodPost, "/api/server/mods/42/ack"); code != 404 {
		t.Errorf("unknown mod ack = %d, want 404", code)
	}
}

// TestDetailsIPLocation checks that the Details dialog's IPs come with
// where GeoLite2-City puts them, for the map, and none for an unknown IP.
func TestDetailsIPLocation(t *testing.T) {
	db := testDB(t)
	path := filepath.Join(t.TempDir(), geoip.FileName)
	if err := os.WriteFile(path, test.GeoIPCityDatabase(t, "81.2.69.0", "GB", "Leeds", "England", 53.8, -1.55, 20), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := geoip.Load(path); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().Add(-time.Hour)
	for _, ip := range []string{"81.2.69.160", "10.0.0.2"} {
		if err := db.OpenPlayerSession(test.GUID1, "Marrow", ip, 3, t0); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ClosePlayerSession(test.GUID1, t0.Add(time.Minute), "disconnected"); err != nil {
			t.Fatal(err)
		}
		t0 = t0.Add(2 * time.Minute)
	}
	rec := httptest.NewRecorder()
	buildWebRoutes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/players/"+test.GUID1+"/details", nil))
	var d struct {
		Details struct {
			IPs []struct {
				IP       string          `json:"ip"`
				Location *geoip.Location `json:"location"`
			} `json:"ips"`
		} `json:"details"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, i := range d.Details.IPs {
		switch i.IP {
		case "81.2.69.160":
			if i.Location == nil || i.Location.City != "Leeds" || i.Location.RadiusKm != 20 {
				t.Errorf("81.2.69.160: %+v", i.Location)
			}
			found++
		case "10.0.0.2":
			if i.Location != nil {
				t.Errorf("10.0.0.2 has a location: %+v", i.Location)
			}
			found++
		}
	}
	if found != 2 {
		t.Errorf("IPs: %s", rec.Body.String())
	}
}

// TestPopulationAll checks the population graph's All: it starts at the
// oldest session and covers the ones in it.
func TestPopulationAll(t *testing.T) {
	db := testDB(t)
	t0 := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Second)
	if err := db.OpenPlayerSession(test.GUID1, "Marrow", "1.2.3.4", 3, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClosePlayerSession(test.GUID1, t0.Add(time.Hour), "disconnected"); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	buildWebRoutes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/server/population?range=all", nil))
	var d struct {
		OK     bool               `json:"ok"`
		From   int64              `json:"from"`
		Points []serverinfo.Point `json:"points"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	peak := 0
	for _, p := range d.Points {
		peak = max(peak, p.Players)
	}
	if d.From != t0.Unix() || peak != 1 {
		t.Errorf("from %d (want %d), peak %d (want 1)", d.From, t0.Unix(), peak)
	}
}

// TestServerSettingsRoutes checks GET and POST /api/server/settings.
func TestServerSettingsRoutes(t *testing.T) {
	db := testDB(t)
	serverinfo.SetDatabase(db)
	t.Cleanup(func() { serverinfo.SaveSettings(serverinfo.Settings{}); serverinfo.SetDatabase(nil) })
	mux := buildWebRoutes()
	do := func(method, body string) (int, string) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, "/api/server/settings", strings.NewReader(body)))
		return rec.Code, rec.Body.String()
	}
	if code, body := do(http.MethodGet, ""); code != 200 || !strings.Contains(body, `"defaults"`) {
		t.Fatalf("GET: %d %s", code, body)
	}
	if code, _ := do(http.MethodPost, `{"queryPort": "abc"}`); code != 400 {
		t.Errorf("bad port: %d, want 400", code)
	}
	if code, body := do(http.MethodPost, `{"queryPort": "27999", "latitude": "33"}`); code != 200 || !strings.Contains(body, `"queryPort":"27999"`) {
		t.Errorf("save: %d %s", code, body)
	}
}

// TestChatCommandsGetHasVehicleFields checks GET /api/chat-commands says
// whether the mod is connected, lists the vehicles missing from the map's
// vehicles.json, and marks the vehicle commands as needing the mod.
func TestChatCommandsGetHasVehicleFields(t *testing.T) {
	testDB(t)
	rec := httptest.NewRecorder()
	buildWebRoutes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/chat-commands", nil))
	var out struct {
		ModConnected     *bool             `json:"modConnected"`
		UnlistedVehicles []json.RawMessage `json:"unlistedVehicles"`
		Commands         []struct {
			Cmd      string `json:"cmd"`
			NeedsMod bool   `json:"needsMod"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.ModConnected == nil || *out.ModConnected || out.UnlistedVehicles == nil || len(out.UnlistedVehicles) != 0 {
		t.Errorf("got %s", rec.Body.String())
	}
	for _, c := range out.Commands {
		if c.Cmd == "car" && !c.NeedsMod {
			t.Error("!car doesn't need the mod")
		}
	}
}

// TestStatusHasVersions checks GET /api/status says ReCON's version (set at
// build time) and the connected mod's (empty while it isn't connected), for
// the version line at the bottom right of every page.
func TestStatusHasVersions(t *testing.T) {
	old := Version
	Version = "v9.9.9-test"
	t.Cleanup(func() { Version = old })
	rec := httptest.NewRecorder()
	buildWebRoutes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	var out struct {
		Version *string `json:"version"`
		Mod     *string `json:"mod"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Version == nil || *out.Version != "v9.9.9-test" || out.Mod == nil || *out.Mod != "" {
		t.Errorf("got %s", rec.Body.String())
	}
}
