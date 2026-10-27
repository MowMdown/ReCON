package rcon

import (
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
)

// environmentOrDefault returns the value of the environment variable `key`,
// or `def` when the variable is unset or empty.
func environmentOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Settings is where ReCON finds the game server's RCON. The defaults
// come from the environment (docker-compose.yaml): RCON_HOST (default
// 127.0.0.1), RCON_PORT (default 2306) and RCON_PASSWORD. RCON Settings on
// the Players page can override any of them until ReCON restarts: the
// overrides are kept in memory only (rconOverrides), and a field left empty
// uses the default. The CLI uses the environment only.
type Settings struct {
	Host     string `json:"host"`
	Port     string `json:"port"`
	Password string `json:"password"`
}

// rconOverrides is what RCON Settings last connected with (all empty until
// then).
var rconOverrides struct {
	sync.Mutex
	settings Settings
}

// DefaultSettings are the settings from the environment.
func DefaultSettings() Settings {
	return Settings{
		Host:     environmentOrDefault("RCON_HOST", "127.0.0.1"),
		Port:     environmentOrDefault("RCON_PORT", "2306"),
		Password: os.Getenv("RCON_PASSWORD"),
	}
}

// clean trims the fields and checks them. Empty fields are fine (they use
// the defaults). Errors are user-facing.
func (s Settings) Clean() (Settings, error) {
	s.Host, s.Port = strings.TrimSpace(s.Host), strings.TrimSpace(s.Port)
	if strings.ContainsAny(s.Host, " \t/") {
		return s, errors.New("Server IP: enter an IP address or host name, such as 10.0.0.5")
	}
	if s.Port != "" {
		if n, err := strconv.Atoi(s.Port); err != nil || n < 1 || n > 65535 {
			return s, errors.New("Port: enter a number from 1 to 65535")
		}
	}
	return s, nil
}

// effective fills the empty fields from the defaults and returns the RCON
// endpoint ("host:port") and password to use.
func (s Settings) Effective() (address, password string) {
	d := DefaultSettings()
	if s.Host == "" {
		s.Host = d.Host
	}
	if s.Port == "" {
		s.Port = d.Port
	}
	if s.Password == "" {
		s.Password = d.Password
	}
	return net.JoinHostPort(s.Host, s.Port), s.Password
}

// CurrentOverrides is what RCON Settings last connected with.
func CurrentOverrides() Settings {
	rconOverrides.Lock()
	defer rconOverrides.Unlock()
	return rconOverrides.settings
}

// KeepOverrides remembers the settings RCON Settings connected with,
// until ReCON restarts.
func KeepOverrides(s Settings) {
	rconOverrides.Lock()
	rconOverrides.settings = s
	rconOverrides.Unlock()
}

// rconTarget is the game server's RCON endpoint ("host:port") and password:
// the docker-compose settings, or what was saved in RCON Settings on the
// Players page (Settings). It changes while ReCON runs when
// RCON Settings connects somewhere new, so it is read under its lock.
var rconTarget struct {
	sync.RWMutex
	address, password string
}

// Target returns the RCON endpoint and password new sessions use.
func Target() (address, password string) {
	rconTarget.RLock()
	defer rconTarget.RUnlock()
	return rconTarget.address, rconTarget.password
}

// SetTarget changes the RCON endpoint and password new sessions use.
func SetTarget(address, password string) {
	rconTarget.Lock()
	rconTarget.address, rconTarget.password = address, password
	rconTarget.Unlock()
}
