// Package geoip looks up the country of an IP address, for the flags next to
// IPs on the Players page, in MaxMind's free GeoLite2-Country database.
//
// The database is GeoLite2-Country.mmdb in GEOIP_DIR (default the folder
// players.db is in, so /data). With MAXMIND_ACCOUNT_ID and
// MAXMIND_LICENSE_KEY set, ReCON downloads it when it is missing or older
// than refreshAfter, and checks again every checkEvery; MaxMind updates it
// twice a week. Without them nothing is downloaded: a database already in the
// folder is still used, and with none there are simply no flags. Lookups are
// in memory; an IP the database doesn't cover (private and LAN addresses)
// has no country.
package geoip

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/oschwald/maxminddb-golang"
)

// FileName is the database's name in the folder.
const FileName = "GeoLite2-Country.mmdb"

var (
	// downloadURL is MaxMind's download for GeoLite2-Country, with the
	// account ID and licence key as basic auth.
	downloadURL = "https://download.maxmind.com/geoip/databases/GeoLite2-Country/download?suffix=tar.gz"
	// refreshAfter is how old the file may get before it is downloaded again.
	refreshAfter = 7 * 24 * time.Hour
	// checkEvery is how often its age is checked.
	checkEvery = 24 * time.Hour
	httpClient = &http.Client{Timeout: 2 * time.Minute}
)

// db is the database in use; nil until one is loaded.
var db atomic.Pointer[maxminddb.Reader]

// Config is where the database lives and the MaxMind credentials to download
// it with.
type Config struct {
	Dir, AccountID, LicenseKey string
}

// ConfigFromEnv reads GEOIP_DIR (default dbDir, the folder players.db is
// in), MAXMIND_ACCOUNT_ID and MAXMIND_LICENSE_KEY.
func ConfigFromEnv(dbDir string) Config {
	c := Config{
		Dir:        strings.TrimSpace(os.Getenv("GEOIP_DIR")),
		AccountID:  strings.TrimSpace(os.Getenv("MAXMIND_ACCOUNT_ID")),
		LicenseKey: strings.TrimSpace(os.Getenv("MAXMIND_LICENSE_KEY")),
	}
	if c.Dir == "" {
		c.Dir = dbDir
	}
	return c
}

func (c Config) path() string { return filepath.Join(c.Dir, FileName) }

func (c Config) canDownload() bool { return c.AccountID != "" && c.LicenseKey != "" }

// Start loads the database from c.Dir, if it is there, and with credentials
// keeps it up to date in the background: downloaded now when missing or
// older than refreshAfter, then checked every checkEvery.
func Start(c Config) {
	if err := Load(c.path()); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("geoip: %v", err)
	}
	if !c.canDownload() {
		if db.Load() == nil {
			log.Printf("geoip: no %s in %s and MAXMIND_ACCOUNT_ID / MAXMIND_LICENSE_KEY not set: no country flags", FileName, c.Dir)
		}
		return
	}
	go func() {
		for {
			if err := Refresh(c); err != nil {
				log.Printf("geoip: %v", err)
			}
			time.Sleep(checkEvery)
		}
	}()
}

// Refresh downloads the database when it is missing or older than
// refreshAfter, and loads it.
func Refresh(c Config) error {
	if st, err := os.Stat(c.path()); err == nil && time.Since(st.ModTime()) < refreshAfter {
		return nil
	}
	if err := download(c); err != nil {
		return fmt.Errorf("downloading %s: %w", FileName, err)
	}
	log.Printf("geoip: downloaded %s to %s", FileName, c.Dir)
	return Load(c.path())
}

// download fetches MaxMind's tar.gz and writes the .mmdb inside it to the
// folder, replacing the old one only once the new one is complete.
func download(c Config) error {
	req, err := http.NewRequest(http.MethodGet, downloadURL, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.AccountID, c.LicenseKey)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("MaxMind answered %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("no %s in the download", FileName)
		}
		if err != nil {
			return err
		}
		if filepath.Base(h.Name) != FileName {
			continue
		}
		if err := os.MkdirAll(c.Dir, 0o755); err != nil {
			return err
		}
		tmp := c.path() + ".tmp"
		f, err := os.Create(tmp)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, tr)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(tmp)
			return err
		}
		return os.Rename(tmp, c.path())
	}
}

// Load reads the database at path into memory and starts using it.
func Load(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	r, err := maxminddb.FromBytes(data)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	db.Store(r)
	return nil
}

// Country returns the ISO country code of ip, lower case ("gb"), or "" when
// there is no database or it doesn't know the address.
func Country(ip string) string {
	r := db.Load()
	addr := net.ParseIP(strings.TrimSpace(ip))
	if r == nil || addr == nil {
		return ""
	}
	var rec struct {
		Country struct {
			ISOCode string `maxminddb:"iso_code"`
		} `maxminddb:"country"`
	}
	if err := r.Lookup(addr, &rec); err != nil {
		return ""
	}
	return strings.ToLower(rec.Country.ISOCode)
}
