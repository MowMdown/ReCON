package livemap

import (
	"strings"
	"unicode"
)

// The mod sends the map's place names in the DayZ server's language, so a
// server running in Russian sends Черногорск for Chernogorsk. latinName
// writes such a name in Latin letters, the way the English map spells them
// (Chernogorsk, Elektrozavodsk, Stary Sobor); a name with no Cyrillic in it
// is left as it is.

// placeEnglish is the English for names that are ordinary Russian words
// rather than names, matched in lower case after the whole name is trimmed.
var placeEnglish = map[string]string{
	"военная база":                         "Military Base",
	"аэродром":                             "Airfield",
	"авиационная база выбор":               "Vybor Air Base",
	"карьер красная заря":                  "Krasnaya Zarya Quarry",
	"свято-романовский горнолыжный курорт": "Svyato-Romanovsky Ski Resort",
	"сквсч биатлон арена":                  "SKVSCh Biathlon Arena",
}

// cyrLatin is each lower-case Russian letter in Latin letters.
var cyrLatin = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "yo",
	'ж': "zh", 'з': "z", 'и': "i", 'й': "y", 'к': "k", 'л': "l", 'м': "m",
	'н': "n", 'о': "o", 'п': "p", 'р': "r", 'с': "s", 'т': "t", 'у': "u",
	'ф': "f", 'х': "kh", 'ц': "ts", 'ч': "ch", 'ш': "sh", 'щ': "shch",
	'ъ': "", 'ы': "y", 'ь': "", 'э': "e", 'ю': "yu", 'я': "ya",
	'і': "i", 'ї': "yi", 'є': "ye", 'ґ': "g",
}

// latinLookalike is the Latin letter a Cyrillic one is sometimes typed as
// (Kарьер starts with a Latin K), for matching placeEnglish.
var latinLookalike = map[rune]rune{
	'a': 'а', 'c': 'с', 'e': 'е', 'k': 'к', 'o': 'о', 'p': 'р', 'x': 'х', 'y': 'у',
}

func hasCyrillic(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Cyrillic, r) {
			return true
		}
	}
	return false
}

// latinName is name in Latin letters (see above).
func latinName(name string) string {
	if !hasCyrillic(name) {
		return name
	}
	key := []rune(strings.ToLower(strings.TrimSpace(name)))
	for i, r := range key {
		if c, ok := latinLookalike[r]; ok {
			key[i] = c
		}
	}
	if en, ok := placeEnglish[string(key)]; ok {
		return en
	}
	rs := []rune(name)
	var b strings.Builder
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		lower := unicode.ToLower(r)
		lat, ok := cyrLatin[lower]
		if !ok {
			b.WriteRune(r)
			continue
		}
		var next rune
		if i+1 < len(rs) {
			next = unicode.ToLower(rs[i+1])
		}
		switch {
		// ый and ий end a word as y and iy: Stary, Tikhiy.
		case (lower == 'ы' || lower == 'и') && next == 'й':
			if lower == 'ы' {
				lat = "y"
			} else {
				lat = "iy"
			}
			i++
		// е after ь or ъ is ye: Заболотье, Zabolotye.
		case lower == 'е' && i > 0 && (unicode.ToLower(rs[i-1]) == 'ь' || unicode.ToLower(rs[i-1]) == 'ъ'):
			lat = "ye"
		}
		if lat == "" {
			continue
		}
		// A capital letter, or the first letter of a word, starts with a
		// capital: Черная Поляна, Chernaya Polyana; Зеленая гора, Zelenaya Gora.
		startsWord := i == 0 || !unicode.IsLetter(rs[i-1]) && rs[i-1] != '\''
		if unicode.IsUpper(r) || startsWord {
			lat = strings.ToUpper(lat[:1]) + lat[1:]
		}
		b.WriteString(lat)
	}
	return b.String()
}

// latinPlaces is places with their names in Latin letters.
func latinPlaces(ps []Place) []Place {
	out := make([]Place, len(ps))
	for i, p := range ps {
		p.Name = latinName(p.Name)
		out[i] = p
	}
	return out
}
