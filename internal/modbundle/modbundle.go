// Package modbundle puts the recon-mod this ReCON needs into its data
// folder (<data>/@ReCON), ready to copy to the game server. The image
// carries it at /opt/recon/@ReCON (the release named in recon-mod.version,
// with that tag in a hidden .version file); it is written again whenever
// the data folder's copy is another version.
package modbundle

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DefaultBundle is where the image keeps the mod; RECON_MOD_BUNDLE moves it.
const DefaultBundle = "/opt/recon/@ReCON"

// BundleDir is the bundle's folder for this run.
func BundleDir() string {
	if d := os.Getenv("RECON_MOD_BUNDLE"); d != "" {
		return d
	}
	return DefaultBundle
}

func version(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, ".version"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Install copies bundleDir to dataDir/@ReCON unless it is already that
// version. No bundle (a build outside the image): nothing, no error.
func Install(bundleDir, dataDir string) (written bool, ver string, err error) {
	ver = version(bundleDir)
	if ver == "" {
		return false, "", nil
	}
	dst := filepath.Join(dataDir, "@ReCON")
	if version(dst) == ver {
		return false, ver, nil
	}
	tmp := dst + ".new"
	if err := os.RemoveAll(tmp); err != nil {
		return false, ver, err
	}
	if err := copyTree(bundleDir, tmp); err != nil {
		os.RemoveAll(tmp)
		return false, ver, err
	}
	if err := os.RemoveAll(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, ver, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return false, ver, err
	}
	return true, ver, nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		to := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(to, 0o755)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(to)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}
