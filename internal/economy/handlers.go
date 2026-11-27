package economy

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed templates
var templateFiles embed.FS

type Host struct {
	DB         *sql.DB
	AddRoute   func(string, http.HandlerFunc)
	AddNavLink func(key, label, href string)
	AddQuiet   func(func(*http.Request) bool)
	Page       func(fs.FS, string) http.HandlerFunc
}

func Start(h Host) error {
	if h.DB == nil || h.AddRoute == nil || h.Page == nil {
		return errors.New("economy: ReCON did not provide its database, route registration and page renderer")
	}
	store, err := NewStore(h.DB)
	if err != nil { return err }
	pageFS, err := fs.Sub(templateFiles, "templates")
	if err != nil { return err }
	h.AddRoute("GET /economy", h.Page(pageFS, "economy.html"))
	h.AddRoute("GET /api/economy/status", statusHandler(store))
	h.AddRoute("GET /api/economy/file", fileHandler(store))
	h.AddRoute("POST /api/economy/save", saveHandler(store))
	h.AddRoute("POST /api/economy/validate", validateHandler())
	h.AddRoute("POST /api/economy/import", importHandler(store))
	h.AddRoute("GET /api/economy/export", exportHandler(store))
	if h.AddNavLink != nil { h.AddNavLink("economy", "Economy", "/economy") }
	if h.AddQuiet != nil {
		h.AddQuiet(func(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/api/economy/") })
	}
	return nil
}

func statusHandler(s *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		files, err := s.ListFiles()
		if err != nil { economyError(w, http.StatusInternalServerError, err); return }
		changed := 0
		for _, f := range files { if f.HasChanges { changed++ } }
		writeEconomyJSON(w, http.StatusOK, map[string]any{
			"ok": true, "workspace": WorkspaceStatus{Files: len(files), Changed: changed, Ready: len(files)>0, RestartRequired: changed>0}, "files": files,
		})
	}
}

func fileHandler(s *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := cleanMissionPath(r.URL.Query().Get("path"))
		if name == "" { economyError(w, http.StatusBadRequest, errors.New("valid mission-relative path required")); return }
		source, working, err := s.GetFile(name)
		if err != nil { economyError(w, http.StatusNotFound, err); return }
		writeEconomyJSON(w, http.StatusOK, map[string]any{"ok": true, "path": name, "source": source, "xml": working, "sourceHash": contentHash(source), "workingHash": contentHash(working)})
	}
}

func saveHandler(s *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct { Path string `json:"path"`; XML string `json:"xml"`; SourceHash string `json:"sourceHash"` }
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxXMLBytes+8192)).Decode(&req); err != nil { economyError(w, http.StatusBadRequest, errors.New("invalid request body")); return }
		name := cleanMissionPath(req.Path)
		if name == "" { economyError(w, http.StatusBadRequest, errors.New("valid mission-relative path required")); return }
		if err := s.SaveWorkingCopy(name, req.XML, req.SourceHash); err != nil { economyError(w, http.StatusConflict, err); return }
		writeEconomyJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "Working copy saved in ReCON. Publish to the mission separately."})
	}
}

func validateHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct { Path string `json:"path"`; XML string `json:"xml"` }
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxXMLBytes+8192)).Decode(&req); err != nil { economyError(w, http.StatusBadRequest, errors.New("invalid request body")); return }
		writeEconomyJSON(w, http.StatusOK, map[string]any{"ok": true, "result": ValidateFile(req.Path, req.XML)})
	}
}

func importHandler(s *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
		if err := r.ParseMultipartForm(64 << 20); err != nil { economyError(w, http.StatusBadRequest, errors.New("invalid or oversized import")); return }
		if r.MultipartForm != nil { defer r.MultipartForm.RemoveAll() }
		files := r.MultipartForm.File["files"]
		if len(files) == 0 { economyError(w, http.StatusBadRequest, errors.New("select XML files or one ZIP archive")); return }
		imported := 0
		for _, header := range files {
			f, err := header.Open()
			if err != nil { economyError(w, http.StatusBadRequest, err); return }
			data, readErr := io.ReadAll(io.LimitReader(f, (64<<20)+1))
			_ = f.Close()
			if readErr != nil { economyError(w, http.StatusBadRequest, readErr); return }
			if len(data) > 64<<20 { economyError(w, http.StatusRequestEntityTooLarge, errors.New("import file exceeds 64 MiB")); return }
			if strings.EqualFold(path.Ext(header.Filename), ".zip") {
				n, err := importZip(s, data)
				if err != nil { economyError(w, http.StatusBadRequest, err); return }
				imported += n
			} else {
				name := cleanMissionPath(header.Filename)
				if name == "" || !strings.EqualFold(path.Ext(name), ".xml") { economyError(w, http.StatusBadRequest, fmt.Errorf("unsupported filename %q", header.Filename)); return }
				if err := s.ImportSource(name, string(data)); err != nil { economyError(w, http.StatusBadRequest, err); return }
				imported++
			}
		}
		writeEconomyJSON(w, http.StatusOK, map[string]any{"ok": true, "imported": imported})
	}
}

func importZip(s *Store, data []byte) (int, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil { return 0, fmt.Errorf("invalid ZIP: %w", err) }
	imported := 0
	for _, entry := range reader.File {
		if entry.FileInfo().IsDir() || !strings.EqualFold(path.Ext(entry.Name), ".xml") { continue }
		name := cleanMissionPath(entry.Name)
		if name == "" { return imported, fmt.Errorf("unsafe ZIP path %q", entry.Name) }
		if entry.UncompressedSize64 > maxXMLBytes { return imported, fmt.Errorf("%s exceeds the XML size limit", name) }
		rc, err := entry.Open()
		if err != nil { return imported, err }
		content, readErr := io.ReadAll(io.LimitReader(rc, maxXMLBytes+1))
		_ = rc.Close()
		if readErr != nil { return imported, readErr }
		if len(content) > maxXMLBytes { return imported, fmt.Errorf("%s exceeds the XML size limit", name) }
		if err := s.ImportSource(name, string(content)); err != nil { return imported, err }
		imported++
	}
	if imported == 0 { return 0, errors.New("ZIP contains no XML files") }
	return imported, nil
}

func exportHandler(s *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		files, err := s.ChangedFiles()
		if err != nil { economyError(w, http.StatusInternalServerError, err); return }
		if len(files) == 0 { economyError(w, http.StatusNotFound, errors.New("there are no changed files to export")); return }
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for _, item := range files {
			entry, err := zw.Create(item.Path)
			if err != nil { economyError(w, http.StatusInternalServerError, err); return }
			if _, err := entry.Write([]byte(item.Content)); err != nil { economyError(w, http.StatusInternalServerError, err); return }
		}
		if err := zw.Close(); err != nil { economyError(w, http.StatusInternalServerError, err); return }
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="recon-economy-export.zip"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(buf.Bytes())
	}
}

func cleanMissionPath(value string) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
	value = path.Clean(value)
	if value == "." || value == ".." || strings.HasPrefix(value, "../") || strings.HasPrefix(value, "/") || strings.Contains(value, ":") { return "" }
	return value
}
func writeEconomyJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func economyError(w http.ResponseWriter, status int, err error) {
	writeEconomyJSON(w, status, map[string]any{"ok": false, "error": err.Error()})
}
