package steam

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MowMdown/ReCON/internal/test"
)

func TestSteamBatchTiming(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	due := steamBatchDue(t0, time.Time{}, 1, t0)
	if !due.Equal(t0.Add(time.Minute)) {
		t.Fatalf("first join: due %v, want a minute later", due)
	}
	due = steamBatchDue(t0, due, 2, t0.Add(10*time.Second))
	if !due.Equal(t0.Add(90 * time.Second)) {
		t.Fatalf("second join: due %v, want 30s more", due)
	}
	for n := 3; n <= 10; n++ {
		due = steamBatchDue(t0, due, n, t0.Add(20*time.Second))
	}
	if !due.Equal(t0.Add(3 * time.Minute)) {
		t.Fatalf("many joins: due %v, want capped at 3 minutes", due)
	}
	now := t0.Add(30 * time.Second)
	if due := steamBatchDue(t0, due, steamBatchMaxPlayers, now); !due.Equal(now) {
		t.Fatalf("%d players: due %v, want now", steamBatchMaxPlayers, due)
	}
}

// TestBanRecord checks the flows' Steam ban lookup: nothing without
// STEAM_API_KEY or a SteamID; otherwise GetPlayerBans, asked once and the
// answer reused; a profile the Steam tab fetched is used without asking.
func TestBanRecord(t *testing.T) {
	db := test.OpenDB(t)
	SetDatabase(db)
	const steamID = "76561197960287930"
	if err := db.OpenPlayerSession(test.GUID1, "Suspect", "1.2.3.4", 4, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.OpenPlayerSession(test.GUID2, "NoSteamID", "1.2.3.5", 5, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSteamID(test.GUID1, steamID); err != nil {
		t.Fatal(err)
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ISteamUser/GetPlayerBans/v1/" || r.URL.Query().Get("steamids") != steamID {
			t.Errorf("unexpected call %s", r.URL)
		}
		calls++
		w.Write([]byte(`{"players":[{"SteamId":"` + steamID + `","NumberOfVACBans":2,"NumberOfGameBans":1,"DaysSinceLastBan":400}]}`))
	}))
	t.Cleanup(srv.Close)
	oldBase, oldKey := steamAPIBase, APIKey
	steamAPIBase = srv.URL
	t.Cleanup(func() {
		steamAPIBase, APIKey = oldBase, oldKey
		banCache.Lock()
		banCache.byID = map[string]bansSeen{}
		banCache.Unlock()
		steamCache.Lock()
		delete(steamCache.byID, steamID)
		steamCache.Unlock()
	})
	ctx := context.Background()

	APIKey = ""
	if _, ok := BanRecord(ctx, test.GUID1); ok || calls != 0 {
		t.Fatalf("without STEAM_API_KEY: ok %v, %d call(s)", ok, calls)
	}
	APIKey = "key"
	if _, ok := BanRecord(ctx, test.GUID2); ok || calls != 0 {
		t.Fatalf("without a SteamID: ok %v, %d call(s)", ok, calls)
	}
	want := Bans{VAC: 2, Game: 1, DaysSinceLast: 400}
	for i := 0; i < 2; i++ {
		if b, ok := BanRecord(ctx, strings.ToUpper(test.GUID1)); !ok || b != want {
			t.Fatalf("lookup %d: %+v %v, want %+v", i+1, b, ok, want)
		}
	}
	if calls != 1 {
		t.Fatalf("%d calls to Steam for two lookups, want 1 (the answer is kept)", calls)
	}
	// A fresh profile from the Steam tab is used as it is.
	banCache.Lock()
	banCache.byID = map[string]bansSeen{}
	banCache.Unlock()
	steamCache.Lock()
	steamCache.byID[steamID] = &steamProfile{SteamID: steamID, Found: true, VACBans: 1, DaysSinceLastBan: 9, FetchedUnix: time.Now().Unix()}
	steamCache.Unlock()
	if b, ok := BanRecord(ctx, test.GUID1); !ok || b != (Bans{VAC: 1, DaysSinceLast: 9}) || calls != 1 {
		t.Fatalf("with a fresh profile: %+v %v, %d calls", b, ok, calls)
	}
}
