package serverinfo

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// mapLatitude is each map's latitude (degrees north) as its world config
// sets it, which DayZ's sun follows. Only maps whose value is known are
// listed; any other map uses defaultLatitude unless SERVER_LATITUDE is set.
var mapLatitude = map[string]float64{
	"chernarusplus": 45, // CfgWorlds latitude = -45 (negative is north)
}

const defaultLatitude = 45

// parseServerDate reads serverDZ.cfg's serverTime: "YYYY/MM/DD/HH/MM"
// (the date the server starts on) or "SystemTime" (the machine's date).
func parseServerDate(s string, now time.Time) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if strings.EqualFold(s, "SystemTime") {
		return now, true
	}
	p := strings.Split(s, "/")
	if len(p) != 5 {
		return time.Time{}, false
	}
	var n [5]int
	for i, v := range p {
		x, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return time.Time{}, false
		}
		n[i] = x
	}
	if n[1] < 1 || n[1] > 12 || n[2] < 1 || n[2] > 31 || n[3] < 0 || n[3] > 23 || n[4] < 0 || n[4] > 59 {
		return time.Time{}, false
	}
	d := time.Date(n[0], time.Month(n[1]), n[2], n[3], n[4], 0, 0, time.UTC)
	if d.Day() != n[2] { // 31 February
		return time.Time{}, false
	}
	return d, true
}

// SunTimes is in-game sunrise and sunset ("05:21", "18:39") on date at the
// latitude: the standard sunrise equation (the sun's center 0.833° below
// the horizon), in local solar time. A polar day or night gives no times
// and polar "day" or "night".
func SunTimes(date time.Time, latitude float64) (rise, set, polar string) {
	rad := math.Pi / 180
	n := float64(date.YearDay())
	decl := -23.44 * math.Cos(2*math.Pi/365*(n+10)) // the sun's declination
	cosH := (math.Sin(-0.833*rad) - math.Sin(latitude*rad)*math.Sin(decl*rad)) /
		(math.Cos(latitude*rad) * math.Cos(decl*rad))
	if cosH < -1 {
		return "", "", "day"
	}
	if cosH > 1 {
		return "", "", "night"
	}
	h := math.Acos(cosH) / rad / 15 // hours from noon
	clock := func(x float64) string {
		m := int(math.Round(x * 60))
		return fmt.Sprintf("%02d:%02d", m/60%24, m%60)
	}
	return clock(12 - h), clock(12 + h), ""
}

// Sun is the day and night the page shows for the server's map.
type Sun struct {
	Sunrise  string  `json:"sunrise"` // "" in a polar day or night
	Sunset   string  `json:"sunset"`
	Polar    string  `json:"polar,omitempty"` // "day" (midnight sun) or "night"
	Date     string  `json:"date"`            // "08-31", the date it is worked out for
	Day      int     `json:"dayOfYear"`       // that date's day of the year, for the sky circle
	Latitude float64 `json:"latitude"`        // degrees north
	MapKnown bool    `json:"mapKnown"`        // the latitude is the map's, not the default
}

// SunFor works out sunrise and sunset for the map and config, at now.
func SunFor(c Config, mapName string, now time.Time) Sun {
	date, ok := parseServerDate(c.ServerTime, now)
	if !ok {
		date = now
	}
	lat, known := c.Latitude, !math.IsNaN(c.Latitude)
	if !known {
		lat, known = mapLatitude[strings.ToLower(mapName)]
		if !known {
			lat = defaultLatitude
		}
	}
	rise, set, polar := SunTimes(date, lat)
	return Sun{Sunrise: rise, Sunset: set, Polar: polar, Date: date.Format("01-02"), Day: date.YearDay(), Latitude: lat, MapKnown: known}
}
