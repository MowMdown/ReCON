package main

import (
	"log"

	"github.com/MowMdown/ReCON/internal/economy"
	"github.com/MowMdown/ReCON/internal/livemap"
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
			QueueMissionWrites: func(files []economy.MissionWrite) ([]int, error) {
				writes := make([]livemap.MissionWrite, 0, len(files))
				for _, file := range files { writes = append(writes, livemap.MissionWrite{Name:file.Name, Text:file.Text}) }
				return livemap.QueueMissionWrites(writes)
			},
			MissionWriteStatuses: func(ids []int) []economy.MissionWriteResult {
				results := livemap.MissionWriteStatuses(ids)
				out := make([]economy.MissionWriteResult, 0, len(results))
				for _, result := range results { out = append(out, economy.MissionWriteResult{ID:result.ID, OK:bool(result.OK), Error:result.Error}) }
				return out
			},
		})
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("the Economy page is at /economy")
	})
}
