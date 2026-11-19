package serverinfo

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"time"
)

// Settings are the Server Info page's Settings: the same as
// QUERY_PORT, SERVER_TIME and SERVER_LATITUDE, as typed. An empty
// field uses the container's value. They are kept in the settings table, so
// they survive restarts.
type Settings struct {
	QueryPort  string `json:"queryPort"`
	ServerTime string `json:"serverTime"`
	Latitude   string `json:"latitude"`
}

const settingsKey = "server_info"

// defaults is the config from the container's variables (ConfigFromEnv).
var defaults Config

// Defaults is the config from the container's variables.
func Defaults() Config {
	state.Lock()
	defer state.Unlock()
	return defaults
}

var saved Settings

// SavedSettings is what was saved on the page ("" fields use the defaults).
func SavedSettings() Settings {
	state.Lock()
	defer state.Unlock()
	return saved
}

// apply is def with the filled-in fields of s, checked.
func (s Settings) apply(def Config) (Config, error) {
	s = Settings{QueryPort: strings.TrimSpace(s.QueryPort),
		ServerTime: strings.TrimSpace(s.ServerTime), Latitude: strings.TrimSpace(s.Latitude)}
	c := def
	if s.QueryPort != "" {
		p, err := strconv.Atoi(s.QueryPort)
		if err != nil || p < 1 || p > 65535 {
			return c, fmt.Errorf("query port %q isn't a port (1 to 65535)", s.QueryPort)
		}
		c.QueryPort = p
	}
	if s.ServerTime != "" {
		if _, ok := parseServerDate(s.ServerTime, time.Now()); !ok {
			return c, fmt.Errorf("server time %q isn't like 2000/8/31/06/00 or SystemTime", s.ServerTime)
		}
		c.ServerTime = s.ServerTime
	}
	if s.Latitude != "" {
		f, err := strconv.ParseFloat(s.Latitude, 64)
		if err != nil || f < -89 || f > 89 || math.IsNaN(f) {
			return c, fmt.Errorf("latitude %q isn't a number between -89 and 89", s.Latitude)
		}
		c.Latitude = f
	}
	return c, nil
}

// SaveSettings checks s, saves it and starts using it: the next query goes
// to the new host and port. Nothing changes if a field is wrong.
func SaveSettings(s Settings) (Config, error) {
	state.Lock()
	def := defaults
	state.Unlock()
	c, err := s.apply(def)
	if err != nil {
		return Config{}, err
	}
	if database != nil {
		b, _ := json.Marshal(s)
		if err := database.SaveStoredSetting(settingsKey, string(b)); err != nil {
			return Config{}, err
		}
	}
	state.Lock()
	if state.cfg.QueryPort != c.QueryPort {
		// Another server: forget the last answer.
		state.snap = Snapshot{}
	}
	state.cfg, saved = c, s
	state.Unlock()
	return c, nil
}

// loadSettings starts with the settings saved on the page, if any.
func loadSettings() {
	if database == nil {
		return
	}
	v, ok, err := database.GetStoredSetting(settingsKey)
	if err != nil || !ok {
		return
	}
	var s Settings
	if err := json.Unmarshal([]byte(v), &s); err != nil {
		log.Printf("serverinfo: saved settings: %v", err)
		return
	}
	state.Lock()
	def := defaults
	state.Unlock()
	c, err := s.apply(def)
	if err != nil {
		log.Printf("serverinfo: saved settings: %v; using the container's", err)
		return
	}
	state.Lock()
	state.cfg, saved = c, s
	state.Unlock()
}
