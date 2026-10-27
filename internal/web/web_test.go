package web

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MowMdown/ReCON/internal/bans"
	"github.com/MowMdown/ReCON/internal/flows"
	"github.com/MowMdown/ReCON/internal/players"
	"github.com/MowMdown/ReCON/internal/rcon"
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
	players.SetDatabase(db)
	bans.SetDatabase(db)
	steam.SetDatabase(db)
	players.Reset()
	return db
}

// resetWeb starts a test with no web session, and closes it and restores
// the RCON target after the test.
func resetWeb(t *testing.T) {
	oldAddr, oldPass := rcon.Target()
	rcon.CloseSession()
	t.Cleanup(func() {
		rcon.SetTarget(oldAddr, oldPass)
		rcon.CloseSession()
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
	if s := rcon.CurrentOverrides(); s != (rcon.Settings{}) {
		t.Fatalf("kept after a failed connect: %+v", s)
	}

	// The right settings connect, are kept (in memory only), and become the target.
	if code, got := call(http.MethodPost, `{"host": " `+host+` ", "port": "`+port+`", "password": "secret"}`); code != 200 || got["ok"] != true {
		t.Fatalf("connect = %d %v", code, got)
	}
	if s := rcon.CurrentOverrides(); s.Host != host || s.Port != port || s.Password != "secret" {
		t.Fatalf("kept = %+v", s)
	}
	if addr, pass := rcon.Target(); addr != net.JoinHostPort(host, port) || pass != "secret" {
		t.Fatalf("target = %s %q", addr, pass)
	}
	if _, ok, _ := database.GetStoredSetting("rcon"); ok {
		t.Fatal("RCON Settings were written to the database")
	}

	// Empty fields use the defaults.
	if addr, pass := (rcon.Settings{Port: "1234"}).Effective(); addr != "10.9.9.9:1234" || pass != "compose-password" {
		t.Fatalf("defaults = %s %q", addr, pass)
	}
}

func TestStatusReportsNextRetry(t *testing.T) {
	resetWeb(t)
	oldNext, oldFails, oldPolled := rcon.NextPoll.Load(), rcon.Fails.Load(), rcon.Polled.Load()
	t.Cleanup(func() {
		rcon.NextPoll.Store(oldNext)
		rcon.Fails.Store(oldFails)
		rcon.Polled.Store(oldPolled)
	})
	rcon.Polled.Store(true)
	now := time.Now()

	// Down, with the next heartbeat 20s from now (see the watch's tests).
	rcon.Fails.Store(rcon.DownAfterMisses)
	rcon.NextPoll.Store(now.Add(20 * time.Second).UnixMilli())

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
	rcon.Fails.Store(0)
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
