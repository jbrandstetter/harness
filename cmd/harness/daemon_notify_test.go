package main

// Daemon Notify Wiring
//
// Governing tests: SPEC-0003 REQ "Operator Notification"; issue #725.
//
// internal/notify tests the dispatcher and the watcher against fakes. None of
// that proves the daemon connects them to anything, and a notifier nobody
// feeds reads exactly like a quiet one — which is how #315 hid give-up. So
// every test here drives what the daemon itself builds (startDaemonNotify,
// startDaemonLoopGuard, startDaemonSessionGuard, wireNotifyReload,
// beginDaemonMetrics) over a real Manager and a real hook script, and checks
// what the hook actually received. The doctor's end of it is in
// doctor_notify_test.go.
//
// No test races the hook against a clock. The hook's timeout is a hang guard
// scaled like the waits, and a wait ends when the dispatcher records the
// delivery finished, however long that takes. A delivery that did not exit 0
// fails the test with its own result and error.
//
// @joestump-agent 09/26/2026 - Added for harness#725.
// @joestump-agent 09/28/2026 - Replaced the fixed 5s hook timeout and the
// file-only poll: under load the recorder was killed before it wrote its
// record, and the test said only "the hook never received" (same class as
// internal/notify, harness#798).

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/prometheus/client_golang/prometheus"
	_ "modernc.org/sqlite"

	"github.com/stump-wtf/harness/internal/attach"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/loopguard"
	"github.com/stump-wtf/harness/internal/metrics"
	"github.com/stump-wtf/harness/internal/notify"
	rt "github.com/stump-wtf/harness/internal/runtrace/runtracetest"
	"github.com/stump-wtf/harness/internal/supervisor"
	"github.com/stump-wtf/harness/internal/testwait"
)

// notifyRecorder is a hook that saves each delivery's stdin and
// HARNESS_NOTIFY_* environment into its first argument's directory.
const notifyRecorder = `#!/bin/sh
dir="$1"
env | grep '^HARNESS_NOTIFY_' > "$dir/$$.env.tmp"
cat > "$dir/$$.json.tmp"
mv "$dir/$$.env.tmp" "$dir/$$.env"
mv "$dir/$$.json.tmp" "$dir/$$.json"
`

type hookDelivery struct {
	payload notify.Payload
	env     string
}

// hookWait is what one recorder delivery is allowed on an idle machine.
// Stretched by testwait.Budget, it is the hook's timeout, so it only ever
// bounds a hang: no test here exercises the timeout. A fixed 5s killed the
// recorder before it wrote its record on a loaded Mac, where XProtect scans
// each freshly written script as it runs.
const hookWait = 10 * time.Second

// newNotifyHook writes the recorder and returns the [notify] table that runs
// it, plus the directory its deliveries land in.
func newNotifyHook(t *testing.T) (core.NotifyConfig, string) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "notify.sh")
	if err := os.WriteFile(script, []byte(notifyRecorder), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	return core.NotifyConfig{
		Command:  []string{script, out},
		Events:   slices.Clone(core.DefaultNotifyEvents),
		Timeout:  testwait.Budget(t, hookWait),
		Cooldown: 15 * time.Minute,
	}, out
}

// testNotifier is the notify pipeline the daemon builds, with the
// dispatcher's log kept: its lines carry each failed delivery's error and
// the hook's own output, which the failure messages quote.
type testNotifier struct {
	*daemonNotifier
	log *syncBuffer
}

// startTestNotify starts the pipeline over mgr the way runDaemon does, bar
// where the dispatcher logs, and closes it when the test ends.
func startTestNotify(t *testing.T, mgr *supervisor.Manager) *testNotifier {
	t.Helper()
	buf := &syncBuffer{}
	n := &testNotifier{daemonNotifier: startDaemonNotify(mgr, notify.Options{Logger: log.New(buf)}), log: buf}
	t.Cleanup(n.Close)
	return n
}

// finished reads harness_notify_deliveries_total off the dispatcher: how many
// deliveries of each event have finished with each result. The dispatcher
// counts a delivery only once its hook has exited.
func (n *testNotifier) finished(t *testing.T) map[string]map[string]int {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(n.d.Collector())
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]int{}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			var event, result string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "event":
					event = l.GetValue()
				case "result":
					result = l.GetValue()
				}
			}
			if out[event] == nil {
				out[event] = map[string]int{}
			}
			out[event][result] = int(m.GetCounter().GetValue())
		}
	}
	return out
}

// requireHookSucceeded fails the test on any delivery whose hook did not
// exit 0. A recorder that was killed or failed wrote nothing, so its missing
// record alone would say only "the hook never received" it.
func (n *testNotifier) requireHookSucceeded(t *testing.T, finished map[string]map[string]int) {
	t.Helper()
	var bad []string
	for event, results := range finished {
		for _, r := range []string{notify.ResultError, notify.ResultTimeout} {
			if results[r] > 0 {
				bad = append(bad, fmt.Sprintf("%s ×%d %s", event, results[r], r))
			}
		}
	}
	if len(bad) == 0 {
		return
	}
	slices.Sort(bad)
	msg := fmt.Sprintf("the hook did not exit 0: %s", strings.Join(bad, ", "))
	if last := n.d.Status().Last; last != nil {
		msg += fmt.Sprintf("\nlast delivery: %s for %q finished %s: %s", last.Event, last.Harness, last.Result, last.Error)
	}
	// The dispatcher counts a delivery just before it logs it, and the log
	// line is what carries the hook's own output.
	deadline := time.Now().Add(testwait.Budget(t, time.Second))
	for !strings.Contains(n.log.String(), "notify: hook failed") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s\ndispatcher log:\n%s", msg, n.log.String())
}

func readHookDeliveries(t *testing.T, dir string) []hookDelivery {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	var out []hookDelivery
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var d hookDelivery
		if err := json.Unmarshal(raw, &d.payload); err != nil {
			t.Fatalf("hook stdin is not JSON: %v\n%s", err, raw)
		}
		env, _ := os.ReadFile(strings.TrimSuffix(f, ".json") + ".env")
		d.env = string(env)
		out = append(out, d)
	}
	return out
}

// hookRecord returns the recorder's record of a delivery of event. Call it
// only once the hook has exited 0 on one: the recorder renames its record
// into place before it exits, so the record must be there.
func hookRecord(t *testing.T, dir, event string) hookDelivery {
	t.Helper()
	var got []string
	for _, d := range readHookDeliveries(t, dir) {
		if d.payload.Event == event {
			return d
		}
		got = append(got, d.payload.Event)
	}
	t.Fatalf("the hook exited 0 on a %q delivery, but the recorder holds no record of it (it holds %v)", event, got)
	return hookDelivery{}
}

// waitHookEvent waits until the dispatcher has finished a delivery of event
// and returns what the hook received. It waits on the dispatcher's own count
// of finished deliveries, not the recorder's files, so a delivery that did
// not exit 0 fails here with its own result and error instead of expiring
// the wait as "never received".
//
// The budget is a hang guard. It scales with the go test deadline
// (testwait.Budget), like the other waits in this suite: on a loaded CI
// runner the daemon runs far slower than a laptop, and a fixed 15s expired
// before the stop+notify path was scheduled at all (main went red twice on
// this after #649 landed).
//
// 15s is not enough when two full suites share the runner. The loop guard
// trips on 8 sequential tool events, and the observer feeds it one scan at
// a time; on 09/29 (runs 15437 and 15448, two suites overlapping) it
// managed one event per ~20s, so 8 events needed ~160s while the wait gave
// up at 60s. The guard saw the events creep in (seen=1..3, trips=[]) and
// the delivery never came. 90s at the usual 4x scale is ~6m: about twice
// the worst load seen, and only paid on a genuinely stuck run.
//
// The variadic diagnostics run once at the timeout and are appended to the
// failure, so a red run on the runner says where the event died — observer
// counters (delivered, dropped, unattributed, parse errors) or the guard's
// view (events seen, trips issued) — instead of leaving the question open.
func waitHookEvent(t *testing.T, n *testNotifier, dir, event string, diagnostics ...func() string) hookDelivery {
	t.Helper()
	budget := testwait.Budget(t, 90*time.Second)
	deadline := time.Now().Add(budget)
	for {
		finished := n.finished(t)
		n.requireHookSucceeded(t, finished)
		if finished[event][notify.ResultOK] > 0 {
			return hookRecord(t, dir, event)
		}
		if time.Now().After(deadline) {
			msg := fmt.Sprintf("no %q delivery finished within %v (finished: %v)", event, budget, finished)
			for _, d := range diagnostics {
				if s := d(); s != "" {
					msg += "\n" + s
				}
			}
			t.Fatalf("%s\ndispatcher log:\n%s", msg, n.log.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The claude-rc incident, end to end through the daemon's construction: a
// harness that prints why it cannot run and exits 1 on every start gives up
// into `failed`, and the hook is told — with that line as the cause. When the
// operator fixes it and restarts, the hook hears it recovered.
func TestDaemonNotifyFiresOnGiveUpAndRecovery(t *testing.T) {
	tmp := t.TempDir()
	nc, out := newNotifyHook(t)
	fixed := filepath.Join(tmp, "logged-in")

	reg := attach.NewRegistry(100)
	opts := daemonManagerOptions(reg)
	opts.StatePath = filepath.Join(tmp, "state.json")
	opts.LogDir = filepath.Join(tmp, "logs")
	// Only the durations shrink; MaxRestarts is the daemon's (see
	// TestDaemonPolicyParksAReliablyFailingHarness).
	opts.Policy.CrashWindow = 20 * time.Millisecond
	opts.Policy.CrashThreshold = 3
	opts.Policy.BackoffBase = 2 * time.Millisecond
	opts.Policy.BackoffCap = 10 * time.Millisecond
	opts.Policy.HealthyRun = 0
	opts.Policy.StopGrace = 80 * time.Millisecond

	script := fmt.Sprintf(`if [ -f %q ]; then exec sleep 30; fi; echo "Error: You must be logged in to use Remote Control."; exit 1`, fixed)
	h := core.Harness{
		Name: "claude-rc", Adapter: "generic", Args: []string{"-c", script},
		Backend: core.BackendNative, Restart: core.RestartOnFailure, RestartDelay: time.Millisecond, Enabled: true,
	}
	cfg := &core.Config{
		Harnesses: map[string]core.Harness{h.Name: h}, HarnessOrder: []string{h.Name},
		Profiles: map[string]core.Profile{}, Notify: nc,
	}
	mgr := supervisor.NewManager(cfg, opts)
	reg.SetController(mgr)
	t.Cleanup(mgr.Close)
	n := startTestNotify(t, mgr)

	if !mgr.Start(h.Name) {
		t.Fatal("Start returned false for a configured harness")
	}
	got := waitHookEvent(t, n, out, core.NotifyFailed)
	p := got.payload
	wantMsg := fmt.Sprintf(`claude-rc failed: gave up after %d consecutive failures (last exit 1): "Error: You must be logged in to use Remote Control." — restart with `+
		"`harness restart claude-rc`; see `harness logs claude-rc`", opts.Policy.MaxRestarts+1)
	if p.Message != wantMsg {
		t.Errorf("message = %q\nwant      %q", p.Message, wantMsg)
	}
	if p.Harness != h.Name || p.State != "failed" || p.Cause != "Error: You must be logged in to use Remote Control." ||
		p.ExitCode == nil || *p.ExitCode != 1 || p.Hint != "harness logs claude-rc" {
		t.Fatalf("failed payload = %+v", p)
	}
	for _, want := range []string{
		"HARNESS_NOTIFY_EVENT=failed", "HARNESS_NOTIFY_HARNESS=claude-rc", "HARNESS_NOTIFY_STATE=failed",
		"HARNESS_NOTIFY_HOST=" + n.d.Host(),
		`HARNESS_NOTIFY_MESSAGE=claude-rc failed: gave up after`,
		`"Error: You must be logged in to use Remote Control." — restart with ` + "`harness restart claude-rc`",
	} {
		if !strings.Contains(got.env, want) {
			t.Errorf("hook environment lacks %q:\n%s", want, got.env)
		}
	}

	if err := os.WriteFile(fixed, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !mgr.Restart(h.Name) {
		t.Fatal("Restart returned false")
	}
	rec := waitHookEvent(t, n, out, core.NotifyRecovered)
	if rec.payload.Harness != h.Name || !strings.Contains(rec.payload.Message, "running again (was failed)") {
		t.Fatalf("recovered payload = %+v", rec.payload)
	}
}

// The tars review-lane incident (2026-10-04), end to end through the daemon's
// construction: a scheduled one-shot whose every run exits 1 lands in
// `failed` after each, and its next firing retries it. The hook hears each
// run fail with the ledger's streak, and never a give-up or a recovery: the
// harness never gave up, and starting the next firing recovers nothing.
func TestDaemonNotifyTriggeredRunFailureIsNotAGiveUp(t *testing.T) {
	tmp := t.TempDir()
	nc, out := newNotifyHook(t)
	nc.Events = slices.Clone(core.NotifyEvents) // run_failed is opt-in
	nc.Cooldown = time.Millisecond              // see each run's delivery

	reg := attach.NewRegistry(100)
	opts := daemonManagerOptions(reg)
	opts.StatePath = filepath.Join(tmp, "state.json")
	opts.LogDir = filepath.Join(tmp, "logs")

	h := core.Harness{
		Name: "review", Adapter: "generic",
		Args:    []string{"-c", `echo "ERROR Payment Required: You're out of credits."; exit 1`},
		Backend: core.BackendNative, Restart: core.RestartNo,
		Schedule: "CRON_TZ=UTC 5-59/10 * * * *",
	}
	cfg := &core.Config{
		Harnesses: map[string]core.Harness{h.Name: h}, HarnessOrder: []string{h.Name},
		Profiles: map[string]core.Profile{}, Notify: nc,
	}
	mgr := supervisor.NewManager(cfg, opts)
	reg.SetController(mgr)
	t.Cleanup(mgr.Close)
	n := startTestNotify(t, mgr)

	// Two firings, the second only once the first has been reported, so the
	// ledger holds both by the time the second is.
	if !mgr.StartTransient(h.Name) {
		t.Fatal("StartTransient returned false for a configured harness")
	}
	waitHookEvent(t, n, out, core.NotifyRunFailed)
	if !mgr.StartTransient(h.Name) {
		t.Fatal("StartTransient returned false on the second firing")
	}
	deadline := time.Now().Add(testwait.Budget(t, 90*time.Second))
	for n.finished(t)[core.NotifyRunFailed][notify.ResultOK] < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the second run_failed never finished (finished: %v)\n%s", n.finished(t), n.log.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The give-up would have been queued ahead of each run_failed; give a
	// stray one time to land before declaring it absent.
	time.Sleep(200 * time.Millisecond)
	finished := n.finished(t)
	n.requireHookSucceeded(t, finished)
	if len(finished[core.NotifyFailed]) > 0 || len(finished[core.NotifyRecovered]) > 0 {
		t.Fatalf("a one-shot's failed run sent a give-up or a recovery: %v", finished)
	}
	var streak bool
	for _, d := range readHookDeliveries(t, out) {
		if d.payload.Event == core.NotifyRunFailed && strings.Contains(d.payload.Message, "failed (exit 1), 2 in a row") {
			streak = true
		}
	}
	if !streak {
		t.Fatalf("no run_failed named the 2-run streak: %+v", readHookDeliveries(t, out))
	}
}

// The crush-qwen incident: the loop guard the daemon builds stops a looping
// harness, and — through the daemon's wiring, not a hand-built callback — the
// hook hears which tool, how many times, and that it stays down.
func TestDaemonNotifyFiresOnLoopStop(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("CRUSH_GLOBAL_DATA", "")
	t.Setenv("XDG_DATA_HOME", "")
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "crush"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	work := filepath.Join(tmp, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	nc, out := newNotifyHook(t)

	reg := attach.NewRegistry(100)
	opts := daemonManagerOptions(reg)
	opts.StatePath = filepath.Join(tmp, "state.json")
	opts.LogDir = filepath.Join(tmp, "logs")
	opts.Policy.StopGrace = 100 * time.Millisecond
	h := core.Harness{Name: "crush-qwen", Adapter: "crush", Workdir: work, Backend: core.BackendNative, Restart: core.RestartAlways, Enabled: true}
	cfg := &core.Config{Harnesses: map[string]core.Harness{h.Name: h}, HarnessOrder: []string{h.Name}, Profiles: map[string]core.Profile{}, Notify: nc}
	mgr := supervisor.NewManager(cfg, opts)
	reg.SetController(mgr)
	t.Cleanup(mgr.Close)
	n := startTestNotify(t, mgr)
	if !mgr.Start(h.Name) {
		t.Fatal("Start returned false for a configured harness")
	}
	waitFor(t, "the harness to come up", func() bool {
		snap, _ := mgr.Snapshot(h.Name)
		return snap.State == core.StateRunning && snap.PID != 0
	})

	db := filepath.Join(work, ".crush", "crush.db")
	now := time.Now()
	rt.WriteCrushDB(t, db, rt.CrushSession{
		ID: "live", Created: now, Updated: now,
		Messages: []rt.CrushMessage{{Role: "user", At: now, Parts: `[{"type":"text","data":{"text":"triage harness#383"}}]`}},
	})
	obsOpts := daemonObserverOptions()
	obsOpts.PollInterval = 10 * time.Millisecond
	obs := startDaemonObserver(mgr, obsOpts)
	t.Cleanup(obs.Stop)
	guard := startDaemonLoopGuard(mgr, obs, n.daemonNotifier, loopguard.Options{})
	t.Cleanup(guard.Close)

	incident := map[string]any{"method": "add_comment", "owner": "stump.wtf", "repo": "harness", "index": 383, "body": "."}
	for i := 1; i <= loopguard.DefaultThreshold; i++ {
		id := fmt.Sprintf("c%d", i)
		at := time.Now()
		rt.AppendCrushMessages(t, db, "live",
			rt.CrushMessage{Role: "assistant", At: at, Parts: rt.ToolCall(id, "mcp_gitea_issue_write", incident)},
			rt.CrushMessage{Role: "tool", At: at, Parts: rt.ToolResult(id, fmt.Sprintf(`{"id":%d}`, i))},
		)
	}

	got := waitHookEvent(t, n, out, core.NotifyLoopStopped,
		// harness#740 review: the run-14372 red showed zero deliveries in
		// 124s with nothing in the log between hook activation and failure,
		// so whether the observer ever emitted or the guard ever tripped
		// could not be told apart. These two lines answer it on the next red.
		func() string { return fmt.Sprintf("observer stats: %+v", obs.Stats()) },
		func() string { return fmt.Sprintf("loop guard: seen=%d trips=%+v", guard.Seen(), guard.Trips()) },
	)
	p := got.payload
	if p.Harness != h.Name || p.Tool != "mcp_gitea_issue_write" || p.Count != loopguard.DefaultThreshold || p.State != "stopped" {
		t.Fatalf("loop_stopped payload = %+v", p)
	}
	if !strings.Contains(got.env, "HARNESS_NOTIFY_MESSAGE=crush-qwen stopped by the runaway tool-loop guard: mcp_gitea_issue_write called 8 times in a row") {
		t.Fatalf("hook environment:\n%s", got.env)
	}
}

// The tars incident: the session guard the daemon builds rotates a crush
// harness wedged on context-limit errors, and the hook is told.
func TestDaemonNotifyFiresOnSessionRotation(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "crush"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	work := filepath.Join(tmp, "work")
	nc, out := newNotifyHook(t)

	reg := attach.NewRegistry(100)
	opts := daemonManagerOptions(reg)
	opts.StatePath = filepath.Join(tmp, "state.json")
	opts.LogDir = filepath.Join(tmp, "logs")
	opts.Policy.StopGrace = 100 * time.Millisecond
	h := core.Harness{Name: "crush-qwen", Adapter: "crush", Workdir: work, Backend: core.BackendNative, Restart: core.RestartAlways, Enabled: true}
	cfg := &core.Config{Harnesses: map[string]core.Harness{h.Name: h}, HarnessOrder: []string{h.Name}, Profiles: map[string]core.Profile{}, Notify: nc}
	mgr := supervisor.NewManager(cfg, opts)
	reg.SetController(mgr)
	t.Cleanup(mgr.Close)
	n := startTestNotify(t, mgr)

	writeWedgedCrushStore(t, filepath.Join(work, ".crush", "crush.db"))
	if !mgr.Start(h.Name) {
		t.Fatal("Start returned false for a configured harness")
	}
	waitFor(t, "the harness to come up", func() bool {
		snap, _ := mgr.Snapshot(h.Name)
		return snap.State == core.StateRunning && snap.PID != 0
	})
	g := startDaemonSessionGuard(mgr, n.daemonNotifier, 20*time.Millisecond, 0)
	t.Cleanup(func() { g.Close(); mgr.SetSessionGuard(nil) })

	p := waitHookEvent(t, n, out, core.NotifySessionRotated).payload
	if p.Harness != h.Name || p.State != "running" || p.Cause != "3 of 3 recent turns failed on context-limit errors" ||
		!strings.Contains(p.Message, "restarted on a fresh session") {
		t.Fatalf("session_rotated payload = %+v", p)
	}
}

// writeWedgedCrushStore builds a crush-shaped store whose recent assistant
// turns all failed on a context-limit error.
func writeWedgedCrushStore(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE messages (id INTEGER PRIMARY KEY, session_id TEXT, role TEXT, parts TEXT NOT NULL DEFAULT '[]', model TEXT, created_at INTEGER, updated_at INTEGER)`); err != nil {
		t.Fatal(err)
	}
	const ctxErr = `["{\"type\":\"finish\",\"data\":{\"reason\":\"error\",\"message\":\"Bad Request\",\"details\":\"Prompt exceeds max length\"}}"]`
	now := time.Now()
	for i := 3; i >= 1; i-- {
		at := now.Add(-time.Duration(i) * time.Minute).Unix()
		if _, err := db.Exec(`INSERT INTO messages (role, parts, created_at, updated_at) VALUES ('assistant', ?, ?, ?)`, ctxErr, at, at); err != nil {
			t.Fatal(err)
		}
	}
}

// A [notify] table added by a reload reaches the dispatcher the daemon runs,
// and the composition keeps the hook registered before it.
func TestDaemonNotifyFollowsReload(t *testing.T) {
	tmp := t.TempDir()
	reg := attach.NewRegistry(100)
	opts := daemonManagerOptions(reg)
	opts.StatePath = filepath.Join(tmp, "state.json")
	opts.LogDir = filepath.Join(tmp, "logs")
	cfg := &core.Config{Harnesses: map[string]core.Harness{}, Profiles: map[string]core.Profile{}}
	mgr := supervisor.NewManager(cfg, opts)
	t.Cleanup(mgr.Close)
	n := startTestNotify(t, mgr)

	var prevRan bool
	mgr.SetReloadHook(func() { prevRan = true })
	wireNotifyReload(mgr, n.daemonNotifier)

	nc, out := newNotifyHook(t)
	next := &core.Config{Harnesses: map[string]core.Harness{}, Profiles: map[string]core.Profile{}, Notify: nc}
	mgr.Reload(next)
	if !prevRan {
		t.Fatal("wireNotifyReload replaced the reload hook instead of composing onto it")
	}
	if !n.d.Config().Equal(nc) {
		t.Fatalf("dispatcher config after reload = %+v, want %+v", n.d.Config(), nc)
	}
	n.w.LoopStopped(loopguard.Trip{Harness: "x", Tool: "t", Count: 8})
	waitHookEvent(t, n, out, core.NotifyLoopStopped)
}

// harness_notify_deliveries_total is on the /metrics registry the daemon
// serves, not just on the dispatcher.
func TestDaemonNotifyMetricsRegistered(t *testing.T) {
	tmp := t.TempDir()
	nc, _ := newNotifyHook(t)
	reg := attach.NewRegistry(100)
	opts := daemonManagerOptions(reg)
	opts.StatePath = filepath.Join(tmp, "state.json")
	opts.LogDir = filepath.Join(tmp, "logs")
	mgr := supervisor.NewManager(&core.Config{Harnesses: map[string]core.Harness{}, Profiles: map[string]core.Profile{}, Notify: nc}, opts)
	t.Cleanup(mgr.Close)
	n := startTestNotify(t, mgr)
	dm := beginDaemonMetrics(mgr, metrics.Listener{Addr: "127.0.0.1:0"})
	t.Cleanup(dm.Stop)
	n.registerMetrics(dm)

	if del := n.d.Test(t.Context()); del.Result != notify.ResultOK {
		t.Fatalf("test delivery = %+v\ndispatcher log:\n%s", del, n.log.String())
	}
	families, err := dm.m.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "harness_notify_deliveries_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["event"] == "test" && labels["result"] == "ok" && m.GetCounter().GetValue() == 1 {
				return
			}
		}
		t.Fatalf("harness_notify_deliveries_total has no {event=test,result=ok} 1: %v", f)
	}
	t.Fatal("harness_notify_deliveries_total is not on the daemon's /metrics registry")
}
