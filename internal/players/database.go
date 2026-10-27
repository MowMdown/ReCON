package players

import "github.com/MowMdown/ReCON/internal/store"

// database is the player database, set at startup (SetDatabase).
var database *store.Database

// SetDatabase sets the player database this package uses.
func SetDatabase(db *store.Database) { database = db }
