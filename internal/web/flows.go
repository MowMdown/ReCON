package web

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/MowMdown/ReCON/internal/flows"
)

func handleFlowsList(w http.ResponseWriter, r *http.Request) {
	list, err := flows.ListWithNextRun()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "flows": list, "runs": flows.SnapshotRuns()})
}

// handleFlowSave serves POST /api/flows: with "id" it updates that flow;
// without one (or with "id": null) it creates a new flow, which gets the
// lowest free number from 0. 0 is an ordinary flow number.
func handleFlowSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		flows.Flow
		ID *int64 `json:"id"` // nil: a new flow (shadows Flow.ID when decoding)
	}
	if err := decodeBody(w, r, &req); err != nil {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}
	f := req.Flow
	if req.ID != nil {
		if *req.ID < 0 {
			fail(w, http.StatusBadRequest, "invalid flow id")
			return
		}
		f.ID = *req.ID
	}
	id, err := flows.Save(f, req.ID == nil)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// withFlow loads the flow named by the {id} path value (404 if it doesn't
// exist) and passes it to h.
func withFlow(h func(http.ResponseWriter, *http.Request, flows.Flow)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id < 0 {
			fail(w, http.StatusNotFound, "invalid flow id")
			return
		}
		f, err := flows.Get(id)
		if errors.Is(err, sql.ErrNoRows) {
			fail(w, http.StatusNotFound, "flow not found")
			return
		} else if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		h(w, r, f)
	}
}

// handleFlowRun: optional body {"minutes": N} replaces the flow's delay for
// this run with N minutes.
func handleFlowRun(w http.ResponseWriter, r *http.Request, f flows.Flow) {
	var body struct {
		Minutes int `json:"minutes"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}
	if body.Minutes < 0 || body.Minutes > 10080 {
		fail(w, http.StatusBadRequest, "minutes must be 0-10080")
		return
	}
	if err := flows.StartNow(f, body.Minutes); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, flows.ErrRunning) {
			code = http.StatusConflict
		}
		fail(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func handleFlowStop(w http.ResponseWriter, r *http.Request, f flows.Flow) {
	if err := flows.Stop(f.ID); err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func handleFlowEnable(w http.ResponseWriter, r *http.Request, f flows.Flow) {
	if err := flows.CheckCanEnable(f); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	setFlowEnabled(w, f.ID, true)
}

func handleFlowDisable(w http.ResponseWriter, r *http.Request, f flows.Flow) {
	setFlowEnabled(w, f.ID, false)
}

func setFlowEnabled(w http.ResponseWriter, id int64, on bool) {
	if err := database.SetFlowScheduleEnabled(id, on); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	flows.WakeScheduler()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func handleFlowDelete(w http.ResponseWriter, r *http.Request, f flows.Flow) {
	if err := flows.Delete(f.ID); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, flows.ErrBusy) {
			code = http.StatusConflict
		}
		fail(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// flowFileEntry is one flow in an export file.
type flowFileEntry struct {
	Name     string       `json:"name"`
	Trigger  string       `json:"trigger"`            // schedule, join, chat or manual
	Schedule string       `json:"schedule,omitempty"` // cron, for schedule flows
	Delay    string       `json:"delay,omitempty"`    // e.g. "5s"
	Steps    []flows.Step `json:"steps"`
	OnStop   []flows.Step `json:"onStop,omitempty"`
	// CounterThreshold is the flow's {threshold}; 0 (or left out) means none.
	CounterThreshold int `json:"counterThreshold,omitempty"`
}

type flowFile struct {
	Flows []flowFileEntry `json:"flows"`
}

// handleFlowExport serves the flows, in list order, as a downloadable
// flows.json: every flow, or with ?ids=0,3,5 (the Export dialog's ticked
// flows) only those. Enabled/disabled state is not exported.
func handleFlowExport(w http.ResponseWriter, r *http.Request) {
	var want map[int64]bool // nil: every flow
	if ids := strings.TrimSpace(r.URL.Query().Get("ids")); ids != "" {
		want = map[int64]bool{}
		for _, s := range strings.Split(ids, ",") {
			id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
			if err != nil || id < 0 {
				fail(w, http.StatusBadRequest, "ids: a comma-separated list of flow numbers")
				return
			}
			want[id] = true
		}
	}
	list, err := flows.List()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := flowFile{Flows: make([]flowFileEntry, 0, len(list))}
	for _, f := range list {
		if want != nil && !want[f.ID] {
			continue
		}
		out.Flows = append(out.Flows, flowFileEntry{Name: f.Name, Trigger: f.Trigger, Schedule: f.Schedule,
			Delay: f.Delay, Steps: f.Steps, OnStop: f.OnStop, CounterThreshold: f.CounterThreshold})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="flows.json"`)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	enc.Encode(out)
}

// handleFlowImport appends every flow in a flows.json body to the bottom of
// the list, disabled. A taken name gets " (2)", " (3)", ...; a name over
// flows.MaxNameLen is shortened; flows that fail the usual checks are skipped
// and reported.
func handleFlowImport(w http.ResponseWriter, r *http.Request) {
	var in flowFile
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, "not a valid flows file: "+err.Error())
		return
	}
	if len(in.Flows) == 0 {
		fail(w, http.StatusBadRequest, "no flows in file")
		return
	}
	existing, err := flows.List()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	taken := map[string]bool{}
	for _, f := range existing {
		taken[strings.ToLower(f.Name)] = true
	}
	added := 0
	skipped := []string{}
	for i, e := range in.Flows {
		base := flows.ClipName(strings.TrimSpace(e.Name), flows.MaxNameLen)
		if base == "" {
			skipped = append(skipped, fmt.Sprintf("flow #%d: name is required", i+1))
			continue
		}
		name := base
		for n := 2; taken[strings.ToLower(name)]; n++ {
			suffix := fmt.Sprintf(" (%d)", n)
			name = flows.ClipName(base, flows.MaxNameLen-len(suffix)) + suffix
		}
		if _, err := flows.Save(flows.Flow{Name: name, Trigger: e.Trigger, Schedule: e.Schedule,
			Delay: e.Delay, Steps: e.Steps, OnStop: e.OnStop, CounterThreshold: e.CounterThreshold}, true); err != nil {
			skipped = append(skipped, base+": "+err.Error())
			continue
		}
		taken[strings.ToLower(name)] = true
		added++
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "added": added, "skipped": skipped})
}

// handleFlowPreview returns the edit dialog's plain-English summary.
func handleFlowPreview(w http.ResponseWriter, r *http.Request) {
	var f flows.Flow // trigger, schedule, delay
	if err := decodeBody(w, r, &f); err != nil {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}
	writeJSON(w, http.StatusOK, flows.Preview(f))
}

// handleFlowReorder saves the order of the flows list: {"ids": [...]}.
func handleFlowReorder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if err := decodeBody(w, r, &req); err != nil || len(req.IDs) == 0 {
		fail(w, http.StatusBadRequest, "bad JSON")
		return
	}
	if err := database.ReorderStoredFlows(req.IDs); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
