package main

// livemap.go starts the Live Map page and the API recon-mod, the DayZ
// server-side mod (github.com/MowMdown/ReCON-Mod), talks to.

import (
	"io/fs"
	"log"
	"net/http"
	"path/filepath"

	"github.com/MowMdown/ReCON/internal/game"
	"github.com/MowMdown/ReCON/internal/livemap"
	"github.com/MowMdown/ReCON/internal/store"
	"github.com/MowMdown/ReCON/internal/web"
)

func init() {
	addonStarters = append(addonStarters, func(db *store.Database) {
		err := livemap.Start(livemap.Host{
			DB: db.DB,
			RCONPassword: func() string {
				_, pw := game.Target()
				return pw
			},
			SaveSteamID: db.SetSteamID,
			AddRoute:    web.AddRoute,
			AddNavLink: func(key, label, href string) {
				web.AddNavLink(web.NavLink{Key: key, Label: label, Href: href, Icon: "map-trifold"})
			},
			AddQuiet:    web.AddQuiet,
			Page:        func(files fs.FS, name string) http.HandlerFunc { return web.AddonPage(files, name) },
			SetGameDate: web.SetGameDate,
			DataDir:     filepath.Dir(environmentOrDefault("DB_PATH", "/data/players.db")),
		})
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("the Live Map page is at /live")
	})
}
