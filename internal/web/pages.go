package web

import (
	"embed"
	"html/template"
	"log"
	"net/http"
)

// templateFiles holds templates/*.html, compiled into the binary.
// base.html defines the shared "style" and "reorder" blocks.
//
//go:embed templates/*.html
var templateFiles embed.FS

// loadPageTemplate parses one page together with base.html.
func loadPageTemplate(name string) *template.Template {
	return template.Must(template.New(name).ParseFS(templateFiles, "templates/base.html", "templates/"+name))
}

var (
	playersPage = loadPageTemplate("players.html")
	bansPage    = loadPageTemplate("bans.html")
	flowsPage   = loadPageTemplate("flows.html")
)

// servePageHandler serves an HTML page. Its layout values (widths, paddings, font
// sizes) and polling intervals are written in the page's own HTML.
func servePageHandler(page *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := page.Execute(w, nil); err != nil {
			log.Printf("template %s: %v", page.Name(), err)
		}
	}
}
