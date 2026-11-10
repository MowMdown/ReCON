package geoip

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MowMdown/ReCON/internal/test"
)

// TestCountry checks lookups: an address in the database's network has its
// country, in lower case; others, and anything when there is no database,
// have none.
func TestCountry(t *testing.T) {
	db.Store(nil)
	t.Cleanup(func() { db.Store(nil) })
	if got := Country("81.2.69.160"); got != "" {
		t.Errorf("no database: Country = %q, want none", got)
	}
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, test.GeoIPDatabase(t, "81.2.69.0", "GB"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Load(path); err != nil {
		t.Fatal(err)
	}
	for ip, want := range map[string]string{"81.2.69.160": "gb", " 81.2.69.1 ": "gb", "81.2.70.1": "", "10.0.0.2": "", "not an ip": "", "": ""} {
		if got := Country(ip); got != want {
			t.Errorf("Country(%q) = %q, want %q", ip, got, want)
		}
	}
}

// TestRefresh checks the download: MaxMind's tar.gz is fetched with the
// account ID and licence key, the .mmdb in it saved to the folder and used,
// and a file newer than refreshAfter isn't downloaded again.
func TestRefresh(t *testing.T) {
	db.Store(nil)
	t.Cleanup(func() { db.Store(nil) })
	mmdb := test.GeoIPDatabase(t, "81.2.69.0", "GB")
	var tgz bytes.Buffer
	gz := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "GeoLite2-Country_20261107/COPYRIGHT.txt", Mode: 0o644, Size: 2})
	tw.Write([]byte("hi"))
	tw.WriteHeader(&tar.Header{Name: "GeoLite2-Country_20261107/" + FileName, Mode: 0o644, Size: int64(len(mmdb))})
	tw.Write(mmdb)
	tw.Close()
	gz.Close()

	downloads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); !ok || user != "123456" || pass != "key" {
			http.Error(w, "Invalid license key", http.StatusUnauthorized)
			return
		}
		downloads++
		w.Write(tgz.Bytes())
	}))
	t.Cleanup(srv.Close)
	oldURL := downloadURL
	downloadURL = srv.URL
	t.Cleanup(func() { downloadURL = oldURL })

	c := Config{Dir: t.TempDir(), AccountID: "123456", LicenseKey: "wrong"}
	if err := Refresh(c); err == nil {
		t.Fatal("a wrong licence key didn't fail")
	}
	c.LicenseKey = "key"
	if err := Refresh(c); err != nil {
		t.Fatal(err)
	}
	if got := Country("81.2.69.160"); got != "gb" {
		t.Errorf("after the download: Country = %q, want gb", got)
	}
	if err := Refresh(c); err != nil || downloads != 1 {
		t.Errorf("a fresh file was downloaded again (%d downloads, err %v)", downloads, err)
	}
	old := time.Now().Add(-refreshAfter - time.Hour)
	os.Chtimes(c.path(), old, old)
	if err := Refresh(c); err != nil || downloads != 2 {
		t.Errorf("a week-old file wasn't downloaded again (%d downloads, err %v)", downloads, err)
	}
	if _, err := os.Stat(c.path() + ".tmp"); !os.IsNotExist(err) {
		t.Error("the temporary file was left behind")
	}
}

// TestConfigFromEnv checks the folder default and that downloading needs
// both credentials.
func TestConfigFromEnv(t *testing.T) {
	t.Setenv("GEOIP_DIR", "")
	t.Setenv("MAXMIND_ACCOUNT_ID", "123456")
	t.Setenv("MAXMIND_LICENSE_KEY", "")
	c := ConfigFromEnv("/data")
	if c.Dir != "/data" || c.canDownload() {
		t.Errorf("got %+v (canDownload %v), want /data and no download without a licence key", c, c.canDownload())
	}
	t.Setenv("GEOIP_DIR", "/geo")
	t.Setenv("MAXMIND_LICENSE_KEY", "key")
	if c = ConfigFromEnv("/data"); c.Dir != "/geo" || !c.canDownload() {
		t.Errorf("got %+v, want /geo and download on", c)
	}
}
