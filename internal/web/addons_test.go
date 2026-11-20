package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// An add-on's page, route and nav link, as recon-mod adds them.
func TestAddonPageRouteAndNav(t *testing.T) {
	addons.Lock()
	nav, routes, quiet, gameDate := addons.nav, addons.routes, addons.quiet, addons.gameDate
	addons.nav, addons.routes, addons.quiet, addons.gameDate = nil, nil, nil, nil
	addons.Unlock()
	t.Cleanup(func() {
		addons.Lock()
		addons.nav, addons.routes, addons.quiet, addons.gameDate = nav, routes, quiet, gameDate
		addons.Unlock()
	})

	files := fstest.MapFS{"live.html": {Data: []byte(`<html><head>{{template "style" .}}</head><body><nav>{{template "navlinks" "live"}}</nav>live page</body></html>`)}}
	AddNavLink(NavLink{Key: "live", Label: "Live Map", Href: "/live"})
	AddRoute("GET /live", AddonPage(files, "live.html"))
	AddRoute("GET /api/addon", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("addon api")) })
	AddQuiet(func(r *http.Request) bool { return r.URL.Path == "/api/addon" })
	mux := buildWebRoutes()

	get := func(path string) string {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		return rec.Body.String()
	}
	page := get("/live")
	if !strings.Contains(page, "live page") || !strings.Contains(page, `<a class="active" href="/live">Live Map</a>`) {
		t.Errorf("the add-on page: %s", page)
	}
	if server := get("/server"); !strings.Contains(server, `<a class="active" href="/server">Server Info</a><a href="/live">Live Map</a><a href="/players">`) {
		t.Error("Server Info's nav lacks Live Map after Server Info")
	}
	if get("/api/addon") != "addon api" {
		t.Error("the add-on route")
	}
	if !isQuiet(httptest.NewRequest(http.MethodGet, "/api/addon", nil)) {
		t.Error("the add-on's quiet path is logged")
	}
	if d, ok := addonGameDate(); ok || d != "" {
		t.Error("an in-game date with no add-on giving one")
	}
	SetGameDate(func() (string, bool) { return "2026/11/20/14/05", true })
	if d, ok := addonGameDate(); !ok || d != "2026/11/20/14/05" {
		t.Errorf("game date %q %v", d, ok)
	}
}

// With no add-ons, the nav is the four pages.
func TestNavWithoutAddons(t *testing.T) {
	rec := httptest.NewRecorder()
	buildWebRoutes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/players", nil))
	if !strings.Contains(rec.Body.String(), `<nav><a href="/server">Server Info</a><a class="active" href="/players">Players</a><a href="/bans">Server Bans</a><a href="/flows">Flows</a><a href="#" id="resetCols">`) {
		t.Error("the Players page's nav changed")
	}
}
