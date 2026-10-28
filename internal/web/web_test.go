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

	"github.com/MowMdown/ReCON/internal/flows"
	"github.com/MowMdown/ReCON/internal/game"
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

func TestRootRedirectsToPlayers(t *testing.T) {
	mux := buildWebRoutes()
	for path, want := range map[string]int{"/": http.StatusFound, "/plyers": http.StatusNotFound} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
		if want == http.StatusFound && rec.Header().Get("Location") != "/players" {
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
