package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// flows.go overview
// ----------------------------------------------------------------------------
// A "flow" is a named, saved script of timed RCON steps. Starting a flow
// begins a countdown; each step fires at its own point in that countdown.
//
// Script syntax, one step per line:  <when> | <command>
//
//	60m      | say -1 Server is going to restart in $minutes minutes!
//	3m       | #lock
//	0m       | #shutdown
//	up+10s   | #unlock
//	cancel   | say -1 Restart cancelled.
//	// comment
//
//	<N>m        N minutes before the end of the countdown (0m = the end)
//	up          as soon as RCON can log in again after the server went down
//	up+<N>s     same, but N seconds after it is back
//	cancel      only runs if the flow is cancelled before its 0m steps fire
//
// $minutes in a command is replaced with that step's minutes value.
//
// API:
//	GET  /api/flows                 flows + current/last run of each
//	POST /api/flows                 create/update {id, name, script} (id 0 = new)
//	POST /api/flows/<id>/start      optional body {"minutes": N} (default = longest step)
//	POST /api/flows/<id>/cancel
//	POST /api/flows/<id>/delete
// Page: /flows
//
// Runs live in memory: restarting this program abandons a countdown in progress.
// ============================================================================

// flowReconnectTimeout is how long a flow waits for the server to go down and
// come back before giving up on its "up" steps.
const flowReconnectTimeout = 30 * time.Minute

// ---------------------------------------------------------------------------
// Server up/down tracking (fed by web.go: markServerDown / markServerUp)
// ---------------------------------------------------------------------------

var serverWatch = struct {
	sync.Mutex
	lastDown time.Time
	lastUp   time.Time
}{}

// markServerDown is called by the background poller after several failed polls.
func markServerDown() {
	serverWatch.Lock()
	serverWatch.lastDown = time.Now()
	serverWatch.Unlock()
}

// markServerUp is called whenever the shared RCON session logs in successfully.
func markServerUp() {
	serverWatch.Lock()
	serverWatch.lastUp = time.Now()
	serverWatch.Unlock()
}

// serverBackSince reports whether the server was seen going down after t0 and
// has logged in again since. Requiring "down after t0" stops an earlier,
// unrelated blip from triggering the post-restart steps early.
func serverBackSince(t0 time.Time) bool {
	serverWatch.Lock()
	defer serverWatch.Unlock()
	return serverWatch.lastDown.After(t0) && serverWatch.lastUp.After(serverWatch.lastDown)
}

// ---------------------------------------------------------------------------
// Script parsing
// ---------------------------------------------------------------------------

type flowStep struct {
	Minutes   int // timed steps: minutes before the end
	DelaySecs int // "up" steps: seconds after the server is back
	Command   string
}

type parsedFlow struct {
	Timed    []flowStep // sorted by Minutes, largest first (script order kept for ties)
	OnUp     []flowStep
	OnCancel []string
}

var (
	whenTimed = regexp.MustCompile(`^(\d+)m$`)
	whenUp    = regexp.MustCompile(`^up(?:\+(\d+)s)?$`)
)

func parseFlowScript(script string) (*parsedFlow, error) {
	pf := &parsedFlow{}
	for i, raw := range strings.Split(script, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		when, cmd, ok := strings.Cut(line, "|")
		if !ok {
			return nil, fmt.Errorf("line %d: expected \"<when> | <command>\"", i+1)
		}
		when = strings.ToLower(strings.TrimSpace(when))
		cmd = cleanReason(cmd)
		if cmd == "" {
			return nil, fmt.Errorf("line %d: command is empty", i+1)
		}
		switch {
		case when == "cancel":
			pf.OnCancel = append(pf.OnCancel, cmd)
		case whenTimed.MatchString(when):
			m, err := strconv.Atoi(whenTimed.FindStringSubmatch(when)[1])
			if err != nil || m > 10080 {
				return nil, fmt.Errorf("line %d: minutes must be 0-10080", i+1)
			}
			pf.Timed = append(pf.Timed, flowStep{Minutes: m, Command: cmd})
		case whenUp.MatchString(when):
			d := 0
			if s := whenUp.FindStringSubmatch(when)[1]; s != "" {
				d, _ = strconv.Atoi(s)
			}
			pf.OnUp = append(pf.OnUp, flowStep{DelaySecs: d, Command: cmd})
		default:
			return nil, fmt.Errorf("line %d: unknown time %q (use 10m, up, up+10s or cancel)", i+1, when)
		}
	}
	if len(pf.Timed) == 0 {
		return nil, errors.New("flow needs at least one timed step like \"5m | say -1 hello\"")
	}
	sort.SliceStable(pf.Timed, func(a, b int) bool { return pf.Timed[a].Minutes > pf.Timed[b].Minutes })
	return pf, nil
}

// ---------------------------------------------------------------------------
// Storage
// ---------------------------------------------------------------------------

type Flow struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Script string `json:"script"`
}

const defaultRestartScript = `// Server restart: counts down from 60 minutes
60m | say -1 Server is going to restart in $minutes minutes!
30m | say -1 Server is going to restart in $minutes minutes!
15m | say -1 Server is going to restart in $minutes minutes!
10m | say -1 Server is going to restart in $minutes minutes!
5m  | say -1 Server is going to restart in $minutes minutes!
3m  | say -1 Server is going to restart in $minutes minutes!
3m  | #lock
0m  | #shutdown
up+10s | #unlock
cancel | say -1 Server restart cancelled.
`

// initFlows creates the table and seeds an example restart flow on first run.
func initFlows() error {
	if _, err := globalDatabase.Exec(`
		CREATE TABLE IF NOT EXISTS flows (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			script TEXT NOT NULL
		)`); err != nil {
		return err
	}
	var n int
	if err := globalDatabase.QueryRow("SELECT COUNT(*) FROM flows").Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		_, err := globalDatabase.Exec("INSERT INTO flows (name, script) VALUES (?, ?)", "Server restart", defaultRestartScript)
		return err
	}
	return nil
}

func listFlows() ([]Flow, error) {
	rows, err := globalDatabase.Query("SELECT id, name, script FROM flows ORDER BY name COLLATE NOCASE")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	flows := []Flow{}
	for rows.Next() {
		var f Flow
		if err := rows.Scan(&f.ID, &f.Name, &f.Script); err != nil {
			return nil, err
		}
		flows = append(flows, f)
	}
	return flows, rows.Err()
}

func getFlow(id int64) (Flow, error) {
	var f Flow
	err := globalDatabase.QueryRow("SELECT id, name, script FROM flows WHERE id = ?", id).Scan(&f.ID, &f.Name, &f.Script)
	return f, err
}

func saveFlow(f Flow) (int64, error) {
	if f.ID == 0 {
		res, err := globalDatabase.Exec("INSERT INTO flows (name, script) VALUES (?, ?)", f.Name, f.Script)
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	res, err := globalDatabase.Exec("UPDATE flows SET name = ?, script = ? WHERE id = ?", f.Name, f.Script, f.ID)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, sql.ErrNoRows
	}
	return f.ID, nil
}

// ---------------------------------------------------------------------------
// Runner
// ---------------------------------------------------------------------------

// flowRun is the live (or last finished) run of one flow. Fields are guarded
// by flowRuns' mutex.
type flowRun struct {
	flowID int64
	name   string
	status string // running | waiting for server | done | cancelled | timed out
	active bool
	endsAt time.Time
	log    []string
	cancel context.CancelFunc
}

var flowRuns = struct {
	sync.Mutex
	byFlow map[int64]*flowRun
}{byFlow: map[int64]*flowRun{}}

var errFlowRunning = errors.New("flow is already running")

func (r *flowRun) logf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	log.Printf("flow[%s] %s", r.name, msg)
	flowRuns.Lock()
	r.log = append(r.log, time.Now().Format("15:04:05")+"  "+msg)
	flowRuns.Unlock()
}

func (r *flowRun) set(status string, active bool) {
	flowRuns.Lock()
	r.status, r.active = status, active
	flowRuns.Unlock()
}

type runView struct {
	FlowID     int64    `json:"flowId"`
	Name       string   `json:"name"`
	Status     string   `json:"status"`
	Active     bool     `json:"active"`
	EndsAtUnix int64    `json:"endsAtUnix"`
	Log        []string `json:"log"`
}

func snapshotRuns() []runView {
	flowRuns.Lock()
	defer flowRuns.Unlock()
	out := []runView{}
	for _, r := range flowRuns.byFlow {
		out = append(out, runView{r.flowID, r.name, r.status, r.active, r.endsAt.Unix(), append([]string(nil), r.log...)})
	}
	return out
}

// startFlow parses the flow and launches its countdown. minutes <= 0 means
// "use the longest step"; a smaller value skips the steps that are earlier
// than the chosen start (e.g. start with 10 -> the 60/30/15 steps are skipped).
func startFlow(f Flow, minutes int) error {
	pf, err := parseFlowScript(f.Script)
	if err != nil {
		return err
	}
	total := pf.Timed[0].Minutes
	if minutes > 0 {
		total = minutes
	}
	flowRuns.Lock()
	if old := flowRuns.byFlow[f.ID]; old != nil && old.active {
		flowRuns.Unlock()
		return errFlowRunning
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &flowRun{
		flowID: f.ID, name: f.Name, status: "running", active: true,
		endsAt: time.Now().Add(time.Duration(total) * time.Minute), cancel: cancel,
	}
	flowRuns.byFlow[f.ID] = run
	flowRuns.Unlock()
	go executeFlow(ctx, run, pf, total)
	return nil
}

// sleepUntil waits for t. It returns false if ctx was cancelled first.
func sleepUntil(ctx context.Context, t time.Time) bool {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err() == nil
	}
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-tm.C:
		return true
	}
}

// runFlowCommand sends one step's command. Only #lock/#unlock are retried
// (repeating them is harmless); `say` must not be repeated, and #shutdown is
// expected to go unanswered because the server dies.
func runFlowCommand(run *flowRun, command string, minutes int) {
	cmd := cleanReason(strings.ReplaceAll(command, "$minutes", strconv.Itoa(minutes)))
	lower := strings.ToLower(cmd)
	retry := lower == "#lock" || lower == "#unlock"
	_, acked, err := runShared(cmd, 2*time.Second, retry)
	run.logf("sent %q (acked=%v err=%v)", cmd, acked, err)
}

func executeFlow(ctx context.Context, run *flowRun, pf *parsedFlow, total int) {
	defer run.cancel()
	run.logf("started, %d minute(s) until the end", total)

	for _, st := range pf.Timed {
		if st.Minutes > total {
			run.logf("skipped %q (%dm is earlier than the start)", st.Command, st.Minutes)
			continue
		}
		fireAt := run.endsAt.Add(-time.Duration(st.Minutes) * time.Minute)
		if !sleepUntil(ctx, fireAt) {
			run.logf("cancelled")
			for _, c := range pf.OnCancel {
				runFlowCommand(run, c, 0)
			}
			run.set("cancelled", false)
			return
		}
		runFlowCommand(run, st.Command, st.Minutes)
	}

	if len(pf.OnUp) == 0 {
		run.logf("done")
		run.set("done", false)
		return
	}

	// Wait for the server to go down and RCON to log in again.
	run.set("waiting for server", true)
	run.logf("waiting for the server to go down and come back")
	t0 := time.Now()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	timeout := time.NewTimer(flowReconnectTimeout)
	defer timeout.Stop()
	for back := false; !back; {
		select {
		case <-ctx.Done():
			run.logf("cancelled while waiting for the server")
			run.set("cancelled", false)
			return
		case <-timeout.C:
			run.logf("gave up: server did not come back within %s", flowReconnectTimeout)
			run.set("timed out", false)
			return
		case <-tick.C:
			back = serverBackSince(t0)
		}
	}
	upAt := time.Now()
	run.logf("server is back")
	for _, st := range pf.OnUp {
		if !sleepUntil(ctx, upAt.Add(time.Duration(st.DelaySecs)*time.Second)) {
			run.logf("cancelled")
			run.set("cancelled", false)
			return
		}
		runFlowCommand(run, st.Command, 0)
	}
	run.logf("done")
	run.set("done", false)
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

func decodeBody(w http.ResponseWriter, r *http.Request, dst any) error {
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(dst)
}

// handleFlowsAPI serves GET (list) and POST (create/update) on /api/flows.
func handleFlowsAPI(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		flows, err := listFlows()
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "flows": flows, "runs": snapshotRuns()})
	case http.MethodPost:
		var f Flow
		if err := decodeBody(w, r, &f); err != nil {
			fail(w, http.StatusBadRequest, "bad JSON")
			return
		}
		f.Name = strings.TrimSpace(f.Name)
		if f.Name == "" {
			fail(w, http.StatusBadRequest, "name is required")
			return
		}
		if _, err := parseFlowScript(f.Script); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		id, err := saveFlow(f)
		if err != nil {
			fail(w, http.StatusBadRequest, "could not save (is the name already used?)")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
	default:
		fail(w, http.StatusMethodNotAllowed, "use GET or POST")
	}
}

// handleFlowAction serves POST /api/flows/<id>/<start|cancel|delete>.
func handleFlowAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/flows/"), "/"), "/")
	if len(parts) != 2 {
		fail(w, http.StatusNotFound, "invalid flow action")
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		fail(w, http.StatusNotFound, "invalid flow id")
		return
	}
	f, err := getFlow(id)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, http.StatusNotFound, "flow not found")
		return
	} else if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}

	switch parts[1] {
	case "start":
		var body struct {
			Minutes int `json:"minutes"`
		}
		if r.ContentLength != 0 {
			if err := decodeBody(w, r, &body); err != nil && !errors.Is(err, io.EOF) {
				fail(w, http.StatusBadRequest, "bad JSON")
				return
			}
		}
		if body.Minutes < 0 || body.Minutes > 10080 {
			fail(w, http.StatusBadRequest, "minutes must be 0-10080")
			return
		}
		if err := startFlow(f, body.Minutes); err != nil {
			code := http.StatusBadRequest
			if errors.Is(err, errFlowRunning) {
				code = http.StatusConflict
			}
			fail(w, code, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "cancel":
		flowRuns.Lock()
		run := flowRuns.byFlow[id]
		active := run != nil && run.active
		flowRuns.Unlock()
		if !active {
			fail(w, http.StatusConflict, "flow is not running")
			return
		}
		run.cancel()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "delete":
		flowRuns.Lock()
		run := flowRuns.byFlow[id]
		if run != nil && run.active {
			flowRuns.Unlock()
			fail(w, http.StatusConflict, "cancel the running flow first")
			return
		}
		delete(flowRuns.byFlow, id)
		flowRuns.Unlock()
		if _, err := globalDatabase.Exec("DELETE FROM flows WHERE id = ?", id); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		fail(w, http.StatusNotFound, "unknown action")
	}
}

// ---------------------------------------------------------------------------
// Page
// ---------------------------------------------------------------------------

var flowsPage = template.Must(template.Must(baseTemplate.Clone()).New("flows").Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>DayZ Flows</title>
{{template "style" .}}
<style>
.box.wide { width: min(720px, 95vw); }
textarea.script { font-family: ui-monospace, Consolas, monospace; font-size: 13px; white-space: pre; }
.help { color: #aaa; font-size: 13px; line-height: 1.6; margin-bottom: 10px; }
.run { margin-top: 24px; }
pre.log { background: #1a1a1a; padding: 10px; border-radius: 6px; font-size: 12px; white-space: pre-wrap; margin: 6px 0 0; }
</style>
</head>
<body>
<nav><a href="/players">Players</a><a href="/bans">Server Bans</a><a class="active" href="/flows">Flows</a></nav>
<h1>Flows</h1>
<div id="status">Loading...</div>
<p><button onclick="newFlow()">New flow</button></p>
<table>
	<colgroup>
		<col style="width: 260px">
		<col>
		<col style="width: 300px">
	</colgroup>
	<thead>
		<tr><th>Flow</th><th>Status</th><th>Actions</th></tr>
	</thead>
	<tbody id="flows"></tbody>
</table>
<div id="runs"></div>

<div id="modal" class="modal">
	<div class="box wide">
		<h2 id="title"></h2>
		<label>Name</label>
		<input id="name" type="text">
		<div class="help">
			One step per line: <b>when | command</b><br>
			<b>60m</b> = 60 minutes before the end, <b>0m</b> = the end,
			<b>up</b> / <b>up+10s</b> = when RCON logs in again after the server went down (optionally N seconds later),
			<b>cancel</b> = runs only if the flow is cancelled early. <b>$minutes</b> becomes the step's minutes.
			Lines starting with // are comments.
		</div>
		<textarea id="script" class="script" rows="16" spellcheck="false"></textarea>
		<div class="buttons">
			<button onclick="closeModal()">Cancel</button>
			<button id="save">Save</button>
		</div>
	</div>
</div>
<script>
let flows = [];
let editing = null;

function esc(s) {
	return String(s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
}

async function post(url, body) {
	let r = await fetch(url, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify(body || {})
	});
	return r.json();
}

function fmtRemaining(unix) {
	let s = Math.round(unix - Date.now() / 1000);
	if (s <= 0) return 'now';
	let h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60);
	return (h ? h + 'h ' : '') + m + 'm ' + String(s % 60).padStart(2, '0') + 's';
}

function statusText(run) {
	if (!run) return 'Idle';
	if (run.active && run.status === 'running') return 'Running - ends in ' + fmtRemaining(run.endsAtUnix);
	if (run.active) return 'Running - ' + run.status;
	return 'Last run: ' + run.status;
}

async function refresh() {
	try {
		let r = await fetch('/api/flows');
		let d = await r.json();
		if (!d.ok) {
			document.getElementById('status').textContent = (d.error || 'Error') + ' - retrying...';
			return;
		}
		flows = d.flows || [];
		let runs = {};
		(d.runs || []).forEach(function(x) { runs[x.flowId] = x; });
		document.getElementById('status').textContent = flows.length + ' flow(s)';
		document.getElementById('flows').innerHTML = flows.map(function(f) {
			let run = runs[f.id];
			let buttons = (run && run.active)
				? '<button onclick="cancelFlow(' + f.id + ')">Cancel run</button>'
				: '<button onclick="startFlow(' + f.id + ')">Start</button>' +
				  '<button onclick="editFlow(' + f.id + ')">Edit</button>' +
				  '<button onclick="deleteFlow(' + f.id + ')">Delete</button>';
			return '<tr><td>' + esc(f.name) + '</td><td>' + esc(statusText(run)) + '</td><td>' + buttons + '</td></tr>';
		}).join('');
		document.getElementById('runs').innerHTML = (d.runs || []).map(function(x) {
			return '<div class="run"><b>' + esc(x.name) + '</b> - ' + esc(statusText(x)) +
				'<pre class="log">' + esc((x.log || []).join('\n')) + '</pre></div>';
		}).join('');
	} catch(e) {
		document.getElementById('status').textContent = 'Server unavailable - retrying...';
	}
}

async function startFlow(id) {
	let f = flows.find(function(x) { return x.id === id; });
	let v = prompt('Start "' + f.name + '". Minutes until the end (leave blank for the full length):', '');
	if (v === null) return;
	let minutes = v.trim() === '' ? 0 : parseInt(v, 10);
	if (!Number.isInteger(minutes) || minutes < 0) { alert('Enter a whole number of minutes.'); return; }
	let d = await post('/api/flows/' + id + '/start', { minutes: minutes });
	if (!d.ok) alert(d.error || 'Start failed');
	refresh();
}

async function cancelFlow(id) {
	if (!confirm('Cancel the running flow?')) return;
	let d = await post('/api/flows/' + id + '/cancel');
	if (!d.ok) alert(d.error || 'Cancel failed');
	refresh();
}

async function deleteFlow(id) {
	let f = flows.find(function(x) { return x.id === id; });
	if (!confirm('Delete flow "' + f.name + '"?')) return;
	let d = await post('/api/flows/' + id + '/delete');
	if (!d.ok) alert(d.error || 'Delete failed');
	refresh();
}

function openModal(f, title) {
	editing = f;
	document.getElementById('title').textContent = title;
	document.getElementById('name').value = f.name;
	document.getElementById('script').value = f.script;
	document.getElementById('modal').style.display = 'flex';
}

function newFlow() { openModal({ id: 0, name: '', script: '5m | say -1 Restarting in $minutes minutes!\n0m | #shutdown\n' }, 'New flow'); }

function editFlow(id) {
	let f = flows.find(function(x) { return x.id === id; });
	openModal(f, 'Edit flow');
}

function closeModal() { document.getElementById('modal').style.display = 'none'; }

document.getElementById('save').onclick = async () => {
	let d = await post('/api/flows', {
		id: editing.id,
		name: document.getElementById('name').value,
		script: document.getElementById('script').value
	});
	if (!d.ok) { alert(d.error || 'Save failed'); return; }
	closeModal();
	refresh();
};

refresh();
setInterval(refresh, {{.RefreshInterval.Milliseconds}});
</script>
</body>
</html>`))

func handleFlowsPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := flowsPage.Execute(w, pageSettings); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
}

// setupFlows is the single entry point main.go needs: it creates/seeds the
// flows table and registers the page and API routes on mux.
func setupFlows(mux *http.ServeMux) error {
	if err := initFlows(); err != nil {
		return err
	}
	mux.HandleFunc("/flows", handleFlowsPage)
	mux.HandleFunc("/api/flows", handleFlowsAPI)
	mux.HandleFunc("/api/flows/", handleFlowAction)
	return nil
}
