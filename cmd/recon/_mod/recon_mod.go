package main

// recon_mod.go builds recon-mod's panel (the Live Map page and the API the
// DayZ mod talks to, github.com/MowMdown/ReCON-Mod) into ReCON. It is only
// built when copied next to main.go: Go skips folders starting with "_",
// so a plain build has none of it, and ReCON's go.mod doesn't need
// ReCON-Mod. The Dockerfile copies it with --build-arg RECON_MOD=true;
// by hand:
//
//	cp cmd/recon/_mod/recon_mod.go cmd/recon/
//	go get github.com/MowMdown/ReCON-Mod@latest
//	go build ./cmd/recon

import (
	"io/fs"
	"log"
	"net/http"
	"path/filepath"

	"github.com/MowMdown/ReCON-Mod/panel"

	"github.com/MowMdown/ReCON/internal/game"
	"github.com/MowMdown/ReCON/internal/store"
	"github.com/MowMdown/ReCON/internal/web"
)

func init() {
	addonStarters = append(addonStarters, func(db *store.Database) {
		err := panel.Start(panel.Host{
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
		log.Printf("recon-mod: the Live Map page is at /live")
	})
}
