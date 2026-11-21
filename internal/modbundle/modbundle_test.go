package modbundle

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(path string) string { b, _ := os.ReadFile(path); return string(b) }

// bundle makes a bundled @ReCON at version v.
func bundle(t *testing.T, v string) string {
	b := filepath.Join(t.TempDir(), "@ReCON")
	write(t, filepath.Join(b, ".version"), v+"\n")
	write(t, filepath.Join(b, "addons", "ReCON.pbo"), "pbo "+v)
	write(t, filepath.Join(b, "mod.cpp"), "name = \"ReCON\";")
	return b
}

func TestInstallFresh(t *testing.T) {
	data := t.TempDir()
	written, v, err := Install(bundle(t, "v0.0.1m"), data)
	if err != nil || !written || v != "v0.0.1m" {
		t.Fatalf("%v %q %v", written, v, err)
	}
	if read(filepath.Join(data, "@ReCON", "addons", "ReCON.pbo")) != "pbo v0.0.1m" || read(filepath.Join(data, "@ReCON", ".version")) != "v0.0.1m\n" {
		t.Error("not copied")
	}
}

func TestInstallSame(t *testing.T) {
	data := t.TempDir()
	b := bundle(t, "v0.0.1m")
	Install(b, data)
	write(t, filepath.Join(data, "@ReCON", "sentinel"), "x")
	written, _, err := Install(b, data)
	if err != nil || written || read(filepath.Join(data, "@ReCON", "sentinel")) != "x" {
		t.Errorf("rewrote the same version: %v %v", written, err)
	}
}

func TestInstallNewer(t *testing.T) {
	data := t.TempDir()
	Install(bundle(t, "v0.0.1l"), data)
	write(t, filepath.Join(data, "@ReCON", "stray"), "x")
	written, v, err := Install(bundle(t, "v0.0.1m"), data)
	if err != nil || !written || v != "v0.0.1m" {
		t.Fatalf("%v %q %v", written, v, err)
	}
	if _, err := os.Stat(filepath.Join(data, "@ReCON", "stray")); !os.IsNotExist(err) {
		t.Error("old files left behind")
	}
	if read(filepath.Join(data, "@ReCON", "addons", "ReCON.pbo")) != "pbo v0.0.1m" {
		t.Error("not replaced")
	}
	if _, err := os.Stat(filepath.Join(data, "@ReCON.new")); !os.IsNotExist(err) {
		t.Error("@ReCON.new left behind")
	}
}

func TestInstallNoBundle(t *testing.T) {
	data := t.TempDir()
	written, v, err := Install(filepath.Join(t.TempDir(), "none"), data)
	if err != nil || written || v != "" {
		t.Errorf("%v %q %v", written, v, err)
	}
	if _, err := os.Stat(filepath.Join(data, "@ReCON")); !os.IsNotExist(err) {
		t.Error("wrote something without a bundle")
	}
}
