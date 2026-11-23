package livemap

import "testing"

func TestLatinName(t *testing.T) {
	for in, want := range map[string]string{
		"Chernogorsk":            "Chernogorsk", // already Latin
		"Черногорск":             "Chernogorsk",
		"Электрозаводск":         "Elektrozavodsk",
		"Новая Петровка":         "Novaya Petrovka",
		"Старый Собор":           "Stary Sobor",
		"Солнечный":              "Solnechny",
		"Тихий залив":            "Tikhiy Zaliv",
		"Черная Поляна":          "Chernaya Polyana",
		"Заболотье":              "Zabolotye",
		"Дрожжино":               "Drozhzhino",
		"Хелм":                   "Khelm",
		"Ольша":                  "Olsha",
		"военная база":           "Military Base",
		"аэродром":               "Airfield",
		"Авиационная база Выбор": "Vybor Air Base",
		"Kарьер Красная Заря":    "Krasnaya Zarya Quarry", // Latin K
		"Свято-Романовский Горнолыжный курорт": "Svyato-Romanovsky Ski Resort",
	} {
		if got := latinName(in); got != want {
			t.Errorf("latinName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLatinPlacesKeepsTheRest(t *testing.T) {
	in := []Place{{Name: "Березино", Type: "City", X: 12372, Z: 9760}}
	out := latinPlaces(in)
	if out[0] != (Place{Name: "Berezino", Type: "City", X: 12372, Z: 9760}) {
		t.Errorf("got %+v", out[0])
	}
	if in[0].Name != "Березино" {
		t.Errorf("the mod's list was changed: %+v", in[0])
	}
}
