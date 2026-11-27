package main

import (
	"log"

	"github.com/MowMdown/ReCON/internal/economy"
	"github.com/MowMdown/ReCON/internal/store"
	"github.com/MowMdown/ReCON/internal/web"
)

func init() {
	addonStarters = append(addonStarters, func(db *store.Database) {
		err := economy.Start(economy.Host{
			DB: db.DB,
			AddRoute: web.AddRoute,
			AddNavLink: func(key, label, href string) {
				web.AddNavLink(web.NavLink{Key: key, Label: label, Href: href, Icon: "database"})
			},
			AddQuiet: web.AddQuiet,
			Page: web.AddonPage,
		})
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("the Economy page is at /economy")
	})
}
