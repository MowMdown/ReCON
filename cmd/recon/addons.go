package main

import "github.com/MowMdown/ReCON/internal/store"

// addonStarters start the add-ons built into this binary. An add-on's
// file (such as livemap.go) appends to it in its init.
var addonStarters []func(db *store.Database)

// startAddons starts the add-ons, before the web server, so their pages
// and routes are in place when it starts.
func startAddons(db *store.Database) {
	for _, start := range addonStarters {
		start(db)
	}
}
