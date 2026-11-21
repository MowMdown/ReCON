package web

import (
	"html/template"
	"io/fs"
	"net/http"
	"sync"
)

// Add-ons: code built into ReCON only on request (recon-mod's Live Map,
// see cmd/recon/_mod) adds its pages, API routes and nav links here before
// Serve starts. ReCON itself has none.

// NavLink is an add-on page in every page's nav (after Server Info).
type NavLink struct {
	Key   string // the page's name, which marks it active ("live")
	Label string
	Href  string
	Icon  string // a Phosphor icon name ("map-trifold"), shown before the label
}

var addons struct {
	sync.Mutex
	nav    []NavLink
	routes []addonRoute
	quiet  []func(*http.Request) bool
	// gameDate is the in-game date ("2026/11/20/14/05") when an add-on
	// knows it better than SERVER_TIME (ok false: it doesn't).
	gameDate func() (string, bool)
}

type addonRoute struct {
	pattern string
	handler http.HandlerFunc
}

// AddNavLink adds a page to the nav of every page.
func AddNavLink(l NavLink) {
	addons.Lock()
	defer addons.Unlock()
	addons.nav = append(addons.nav, l)
}

// AddRoute adds a route ("GET /live", "POST /api/mod/sync") to the web
// server. Call it before Serve.
func AddRoute(pattern string, h http.HandlerFunc) {
	addons.Lock()
	defer addons.Unlock()
	addons.routes = append(addons.routes, addonRoute{pattern, h})
}

// AddQuiet leaves the requests f picks out of the request log (an add-on's
// routine polls).
func AddQuiet(f func(*http.Request) bool) {
	addons.Lock()
	defer addons.Unlock()
	addons.quiet = append(addons.quiet, f)
}

// SetGameDate sets where Server Info gets the in-game date from, when it
// knows it (used for sunrise and sunset instead of SERVER_TIME).
func SetGameDate(f func() (string, bool)) {
	addons.Lock()
	defer addons.Unlock()
	addons.gameDate = f
}

// AddonPage is an add-on's HTML page: the template name in files, parsed
// with base.html, so it can use ReCON's "style", "icons", "theme",
// "statusbar" and "navlinks" templates (pass its NavLink Key to
// "navlinks").
func AddonPage(files fs.FS, name string) http.HandlerFunc {
	base := template.Must(template.New(name).Funcs(pageFuncs).ParseFS(templateFiles, "templates/base.html"))
	page := template.Must(template.Must(base.Clone()).ParseFS(files, name))
	return servePageHandler(page)
}

func navExtra() []NavLink {
	addons.Lock()
	defer addons.Unlock()
	return append([]NavLink(nil), addons.nav...)
}

func addonQuiet(r *http.Request) bool {
	addons.Lock()
	quiet := append([]func(*http.Request) bool(nil), addons.quiet...)
	addons.Unlock()
	for _, f := range quiet {
		if f(r) {
			return true
		}
	}
	return false
}

func addonGameDate() (string, bool) {
	addons.Lock()
	f := addons.gameDate
	addons.Unlock()
	if f == nil {
		return "", false
	}
	return f()
}

// pageFuncs are the functions the page templates can use.
var pageFuncs = template.FuncMap{"navExtra": navExtra}
