package notify

// Dispatcher Tests
//
// Governing tests: SPEC-0003 REQ "Operator Notification"; issue #725. Every
// test runs a real hook — a shell script exec'd the way production execs one —
// and reads back what it actually received, so a dispatcher that built the
// right payload and then failed to hand it over cannot pass.
//
// No test races a hook against a clock. Each waits for the delivery itself —
// the dispatcher reports every hook exit to the test — and counts what the
// hook received only after Close has let every queued delivery finish, so a
// loaded machine makes the suite slower, never red. The timeout tests start
// the clock once the hook is provably running.
//
// @joestump-agent 09/28/2026 - Replaced the fixed 5s hook timeout, 10s file
// poll, 1s kill timer and 100ms "nothing else arrived" sleeps, which failed
// correct code under load (the hook was killed before it wrote its record, and
// the test said only "hook ran 0 times").

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/testwait"
)

// recorder is a hook that writes each delivery's HARNESS_NOTIFY_* environment
// and stdin into dir, atomically, one pair of files per run.
const recorder = `#!/bin/sh
dir="$1"
env | grep '^HARNESS_NOTIFY_' > "$dir/$$.env.tmp"
cat > "$dir/$$.json.tmp"
mv "$dir/$$.env.tmp" "$dir/$$.env"
mv "$dir/$$.json.tmp" "$dir/$$.json"
`

// Received is one delivery as the hook saw it.
type Received struct {
	Env     map[string]string
	Payload Payload
	Raw     string
}

// NewRecorder writes the recorder hook into a temp dir and returns its argv
// and the directory deliveries land in.
func NewRecorder(t testing.TB) (argv []string, dir string) {
	t.Helper()
	dir = t.TempDir()
	script := filepath.Join(dir, "hook.sh")
	if err := os.WriteFile(script, []byte(recorder), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{script, out}, out
}

// ReadReceived returns every delivery the recorder has completed.
func ReadReceived(t testing.TB, dir string) []Received {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	slices.Sort(files)
	var out []Received
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var r Received
		r.Raw = string(raw)
		if err := json.Unmarshal(raw, &r.Payload); err != nil {
			t.Fatalf("hook stdin is not JSON: %v\n%s", err, raw)
		}
		envRaw, err := os.ReadFile(strings.TrimSuffix(f, ".json") + ".env")
		if err != nil {
			t.Fatal(err)
		}
		r.Env = map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(string(envRaw)), "\n") {
			k, v, _ := strings.Cut(line, "=")
			r.Env[k] = v
		}
		out = append(out, r)
	}
	return out
}

func quietLogger() *log.Logger { return log.New(nopWriter{}) }

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// hookWait is what a recorder delivery is allowed on an idle machine. The
// tests stretch it with testwait.Budget, and it only ever bounds a hang: a
// wait on a delivery ends when the hook exits, however long that takes.
const hookWait = 10 * time.Second

// testConfig is a [notify] table that runs argv. Its timeout is the hang
// guard, not a race with the hook: the tests that exercise the timeout set
// their own.
func testConfig(t *testing.T, argv []string) core.NotifyConfig {
	return core.NotifyConfig{
		Command:  argv,
		Events:   slices.Clone(core.DefaultNotifyEvents),
		Timeout:  testwait.Budget(t, hookWait),
		Cooldown: time.Minute,
	}
}

// testDispatcher is a Dispatcher that records every delivery it finishes, so
// a test waits on the deliveries themselves instead of polling the hook's
// files against a deadline.
type testDispatcher struct {
	*Dispatcher

	mu       sync.Mutex
	finished []Delivery
	more     chan struct{} // cap 1: a delivery finished since the last look
}

func newTestDispatcher(t *testing.T, cfg core.NotifyConfig, opts Options) *testDispatcher {
	t.Helper()
	if opts.Hostname == nil {
		opts.Hostname = func() (string, error) { return "kitt", nil }
	}
	if opts.ShutdownGrace == 0 {
		// settle uses Close as its barrier, so Close must let every queued
		// delivery run rather than kill what is slow.
		opts.ShutdownGrace = testwait.Budget(t, hookWait)
	}
	opts.Logger = quietLogger()
	td := &testDispatcher{more: make(chan struct{}, 1)}
	opts.delivered = td.record
	td.Dispatcher = New(cfg, opts)
	t.Cleanup(td.Close)
	return td
}

func (td *testDispatcher) record(del Delivery) {
	td.mu.Lock()
	td.finished = append(td.finished, del)
	td.mu.Unlock()
	select {
	case td.more <- struct{}{}:
	default:
	}
}

// outcomes returns every delivery finished so far and fails the test on
// any whose hook did not exit 0: a recorder that was killed or never started
// wrote nothing, and its missing file alone says only "hook ran 0 times".
func (td *testDispatcher) outcomes(t *testing.T) []Delivery {
	t.Helper()
	td.mu.Lock()
	got := slices.Clone(td.finished)
	td.mu.Unlock()
	for _, del := range got {
		if del.Result != ResultOK {
			t.Fatalf("the %s delivery for %q finished %s: %s", del.Event, del.Harness, del.Result, del.Error)
		}
	}
	return got
}

// waitDelivered waits until n deliveries have finished.
func (td *testDispatcher) waitDelivered(t *testing.T, n int) {
	t.Helper()
	budget := testwait.Budget(t, hookWait)
	timeout := time.NewTimer(budget)
	defer timeout.Stop()
	for len(td.outcomes(t)) < n {
		select {
		case <-td.more:
		case <-timeout.C:
			t.Fatalf("%d of %d deliveries finished within %v", len(td.outcomes(t)), n, budget)
		}
	}
}

// settle closes the dispatcher, which returns once every notification it
// accepted has been delivered, and returns what the recorder in dir received,
// failing unless that is exactly want deliveries. Nothing can arrive after
// it, so a test that expects silence can trust it.
func (td *testDispatcher) settle(t *testing.T, dir string, want int) []Received {
	t.Helper()
	start := time.Now()
	td.Close()
	if took := time.Since(start); took >= td.opts.ShutdownGrace {
		t.Fatalf("deliveries were still running %v into Close, which killed them", took)
	}
	dels := td.outcomes(t)
	got := ReadReceived(t, dir)
	if len(got) != len(dels) {
		t.Fatalf("%d hooks exited 0 but the recorder holds %d deliveries", len(dels), len(got))
	}
	if len(got) != want {
		var seen []string
		for _, r := range got {
			seen = append(seen, r.Payload.Event+"/"+r.Payload.Harness)
		}
		t.Fatalf("the hook ran %d times, want %d: %v", len(got), want, seen)
	}
	return got
}

func TestDeliveryEnvAndStdin(t *testing.T) {
	argv, dir := NewRecorder(t)
	// A stale HARNESS_NOTIFY_* in the daemon's own environment must not
	// reach the hook alongside the real one.
	environ := func() []string { return append(os.Environ(), "HARNESS_NOTIFY_EVENT=stale") }
	d := newTestDispatcher(t, testConfig(t, argv), Options{Environ: environ})

	code := 1
	at := time.Date(2026, 9, 25, 20, 15, 16, 0, time.UTC)
	if !d.Notify(Notification{
		Event: core.NotifyFailed, Harness: "claude-rc", State: "failed",
		Message:  `claude-rc failed: "Error: You must be logged in to use Remote Control."`,
		Cause:    "Error: You must be logged in to use Remote Control.",
		Hint:     "harness logs claude-rc",
		Time:     at,
		ExitCode: &code, Restarts: 6,
	}) {
		t.Fatal("Notify refused a wanted event")
	}
	r := d.settle(t, dir, 1)[0]

	for k, want := range map[string]string{
		EnvEvent:   "failed",
		EnvHarness: "claude-rc",
		EnvHost:    "kitt",
		EnvState:   "failed",
		EnvMessage: `claude-rc failed: "Error: You must be logged in to use Remote Control."`,
	} {
		if r.Env[k] != want {
			t.Errorf("%s = %q, want %q", k, r.Env[k], want)
		}
	}
	p := r.Payload
	if p.Version != PayloadVersion || p.Event != "failed" || p.Harness != "claude-rc" || p.Host != "kitt" ||
		p.State != "failed" || p.Cause != "Error: You must be logged in to use Remote Control." ||
		p.Hint != "harness logs claude-rc" || p.Time != "2026-09-25T20:15:16Z" ||
		p.ExitCode == nil || *p.ExitCode != 1 || p.Restarts != 6 {
		t.Fatalf("payload = %+v\n%s", p, r.Raw)
	}
}

// The payload quotes agent output, which is where credentials turn up. They
// must be masked in the environment and on stdin alike.
func TestDeliveryIsRedacted(t *testing.T) {
	argv, dir := NewRecorder(t)
	d := newTestDispatcher(t, testConfig(t, argv), Options{})
	const secret = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	d.Notify(Notification{
		Event: core.NotifyFailed, Harness: "w",
		Message: "w failed: \"git push https://joe:" + secret + "@gitea.example/x.git\"\nsecond line",
		Cause:   "GITEA_TOKEN=" + secret,
	})
	r := d.settle(t, dir, 1)[0]
	if strings.Contains(r.Raw, secret) || strings.Contains(r.Env[EnvMessage], secret) {
		t.Fatalf("secret reached the hook:\nenv: %q\nstdin: %s", r.Env[EnvMessage], r.Raw)
	}
	if !strings.Contains(r.Payload.Message, "[REDACTED]") || !strings.Contains(r.Payload.Cause, "[REDACTED]") {
		t.Fatalf("nothing was masked — the check could not have failed: %+v", r.Payload)
	}
	if strings.Contains(r.Env[EnvMessage], "\n") || !strings.Contains(r.Env[EnvMessage], "second line") {
		t.Fatalf("message not folded onto one line: %q", r.Env[EnvMessage])
	}
}

func TestEventsFilter(t *testing.T) {
	argv, dir := NewRecorder(t)
	cfg := testConfig(t, argv)
	cfg.Events = []string{core.NotifyLoopStopped}
	d := newTestDispatcher(t, cfg, Options{})
	if d.Notify(Notification{Event: core.NotifyFailed, Harness: "a"}) {
		t.Fatal("an event not in `events` was queued")
	}
	if !d.Notify(Notification{Event: core.NotifyLoopStopped, Harness: "a"}) {
		t.Fatal("a listed event was refused")
	}
	if got := d.settle(t, dir, 1); got[0].Payload.Event != core.NotifyLoopStopped {
		t.Fatalf("deliveries = %+v", got)
	}
}

func TestCooldownPerHarnessAndEvent(t *testing.T) {
	argv, dir := NewRecorder(t)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	d := newTestDispatcher(t, testConfig(t, argv), Options{Now: func() time.Time { return now }})

	send := func(event, harness string) bool {
		return d.Notify(Notification{Event: event, Harness: harness})
	}
	if !send(core.NotifyFlapping, "a") {
		t.Fatal("first flapping refused")
	}
	if send(core.NotifyFlapping, "a") {
		t.Fatal("a repeat inside the cooldown was delivered")
	}
	// A different harness, or a different event, is its own key.
	if !send(core.NotifyFlapping, "b") || !send(core.NotifyFailed, "a") {
		t.Fatal("cooldown leaked across harnesses or events")
	}
	now = now.Add(time.Minute)
	if !send(core.NotifyFlapping, "a") {
		t.Fatal("still suppressed after the cooldown elapsed")
	}
	d.settle(t, dir, 4)
	if got := testutil.ToFloat64(d.deliveries.WithLabelValues(core.NotifyFlapping, ResultSuppressed)); got != 1 {
		t.Fatalf("suppressed counter = %v, want 1", got)
	}
}

// A harness brought back by an operator and failing again straight away must
// page again, or "recovered" is the last word on a dead harness.
func TestRecoveredClearsFailedCooldown(t *testing.T) {
	argv, dir := NewRecorder(t)
	d := newTestDispatcher(t, testConfig(t, argv), Options{})
	d.Notify(Notification{Event: core.NotifyFailed, Harness: "a"})
	d.Notify(Notification{Event: core.NotifyRecovered, Harness: "a"})
	if !d.Notify(Notification{Event: core.NotifyFailed, Harness: "a"}) {
		t.Fatal("a failure after recovery was suppressed by the earlier failure's cooldown")
	}
	d.settle(t, dir, 3)
}

func TestCooldownZeroDisablesDedupe(t *testing.T) {
	argv, dir := NewRecorder(t)
	cfg := testConfig(t, argv)
	cfg.Cooldown = 0
	d := newTestDispatcher(t, cfg, Options{})
	for range 3 {
		if !d.Notify(Notification{Event: core.NotifyFlapping, Harness: "a"}) {
			t.Fatal("suppressed with cooldown = 0")
		}
	}
	d.settle(t, dir, 3)
}

// The configured timeout fires on its own: a hook that never finishes is
// killed when it passes and reported as a timeout, whether the kill lands
// before the hook got going or long after, so no timing can fail it.
func TestTimeoutFires(t *testing.T) {
	cfg := testConfig(t, []string{"/bin/sh", "-c", "exec sleep 3600"})
	cfg.Timeout = 100 * time.Millisecond
	d := newTestDispatcher(t, cfg, Options{})
	result := make(chan Delivery, 1)
	go func() { result <- d.Test(context.Background()) }()
	select {
	case del := <-result:
		if del.Result != ResultTimeout || del.Error != "killed after 100ms" {
			t.Fatalf("delivery = %+v, want a timeout", del)
		}
	case <-time.After(testwait.Budget(t, hookWait)):
		t.Fatal("the hook outlived its 100ms timeout")
	}
}

// The timeout kills the hook's whole process group: a script whose child
// keeps the output pipe open must not hold the delivery (or a worker) past
// it, and the child must die with it.
//
// The deadline passes on cue, once the script has recorded its child. A fixed
// 1s timeout raced the script's own startup: on a loaded machine it killed
// the script before the child existed, and the test failed reading a pid file
// that was never written.
func TestTimeoutKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := filepath.Join(dir, "slow.sh")
	// Write-then-rename, since the test reads the pid while the script runs.
	body := "#!/bin/sh\nsleep 3600 &\necho $! > " + pidFile + ".tmp\nmv " + pidFile + ".tmp " + pidFile + "\nwait\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	d := newTestDispatcher(t, testConfig(t, []string{script}), Options{})

	deadline := newCueDeadline()
	defer deadline.pass()
	result := make(chan Delivery, 1)
	go func() { result <- d.Test(deadline) }()
	testwait.Until(t, hookWait, 10*time.Millisecond, "the hook to record its child", func() bool {
		_, err := os.Stat(pidFile)
		return err == nil
	})
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Fatalf("child pid %q: %v", raw, err)
	}
	t.Cleanup(func() {
		if t.Failed() && alive(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	deadline.pass()
	select {
	case del := <-result:
		if del.Result != ResultTimeout {
			t.Fatalf("result = %+v, want timeout", del)
		}
	case <-time.After(testwait.Budget(t, hookWait)):
		t.Fatal("the delivery outlived its deadline: the child held it open")
	}
	testwait.Until(t, hookWait, 10*time.Millisecond, fmt.Sprintf("the hook's child %d to die with it", pid), func() bool {
		return !alive(pid)
	})
	if got := testutil.ToFloat64(d.deliveries.WithLabelValues(core.NotifyTest, ResultTimeout)); got != 1 {
		t.Fatalf("timeout counter = %v, want 1", got)
	}
}

// cueDeadline is a parent context whose deadline passes when the test calls
// pass. The delivery's timeout context inherits it, so the hook is killed
// exactly as when its own timeout fires (DeadlineExceeded, the group kill,
// ResultTimeout), but at a point the test chose rather than a fixed time.
type cueDeadline struct {
	context.Context
	done chan struct{}
	once sync.Once
}

func newCueDeadline() *cueDeadline {
	return &cueDeadline{Context: context.Background(), done: make(chan struct{})}
}

func (c *cueDeadline) pass()                 { c.once.Do(func() { close(c.done) }) }
func (c *cueDeadline) Done() <-chan struct{} { return c.done }

func (c *cueDeadline) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// alive reports whether pid is a running process. A killed child whose
// parent is gone is reparented to PID 1, and in a CI container PID 1 may
// never reap it: it stays a zombie, which kill(pid, 0) still finds. A zombie
// has been killed, which is the property under test.
func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	if stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		// pid (comm) S ... — the state follows the last ')'.
		s := string(stat)
		if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 < len(s) {
			return s[i+2] != 'Z'
		}
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false // ps finds no such process
	}
	st := strings.TrimSpace(string(out))
	return st != "" && !strings.HasPrefix(st, "Z")
}

func TestFailingHookIsReported(t *testing.T) {
	cfg := testConfig(t, []string{"/bin/sh", "-c", "echo boom >&2; exit 3"})
	d := newTestDispatcher(t, cfg, Options{})
	del := d.Test(context.Background())
	if del.Result != ResultError || !strings.Contains(del.Error, "exit status 3") {
		t.Fatalf("delivery = %+v, want an exit-status error", del)
	}
	st := d.Status()
	if st.Last == nil || st.Last.Result != ResultError {
		t.Fatalf("status.Last = %+v, want the failed delivery", st.Last)
	}
	missing := newTestDispatcher(t, testConfig(t, []string{"/nonexistent/notify"}), Options{})
	if del := missing.Test(context.Background()); del.Result != ResultError {
		t.Fatalf("missing hook = %+v, want error", del)
	}
}

// Test ignores `events` and the cooldown: it is how the operator proves the
// hook works, whatever the table filters.
func TestTestIgnoresEventsAndCooldown(t *testing.T) {
	argv, dir := NewRecorder(t)
	cfg := testConfig(t, argv)
	cfg.Events = []string{core.NotifyRunFailed}
	d := newTestDispatcher(t, cfg, Options{})
	for range 2 {
		if del := d.Test(context.Background()); del.Result != ResultOK {
			t.Fatalf("test delivery = %+v", del)
		}
	}
	got := ReadReceived(t, dir)
	if len(got) != 2 || got[0].Payload.Event != core.NotifyTest || got[0].Env[EnvHost] != "kitt" {
		t.Fatalf("deliveries = %+v", got)
	}
	off := newTestDispatcher(t, core.NotifyConfig{}, Options{})
	if del := off.Test(context.Background()); del.Result != ResultError || !strings.Contains(del.Error, "not configured") {
		t.Fatalf("test with notify off = %+v", del)
	}
}

// Notify never blocks: with every worker busy and the queue full, the next
// notification is dropped and counted, not waited for.
func TestFullQueueDropsWithoutBlocking(t *testing.T) {
	// The hook holds its worker until Close kills it, so the worker is busy
	// for the whole test however slowly the machine runs, and a Notify that
	// waited for room in the queue would never return.
	cfg := testConfig(t, []string{"/bin/sh", "-c", "exec sleep 3600"})
	cfg.Cooldown = 0
	d := newTestDispatcher(t, cfg, Options{Workers: 1, Queue: 1, ShutdownGrace: 50 * time.Millisecond})
	type tally struct{ queued, dropped int }
	done := make(chan tally, 1)
	go func() {
		var n tally
		for range 5 {
			if d.Notify(Notification{Event: core.NotifyFlapping, Harness: "a"}) {
				n.queued++
			} else {
				n.dropped++
			}
		}
		done <- n
	}()
	var n tally
	select {
	case n = <-done:
	case <-time.After(testwait.Budget(t, hookWait)):
		t.Fatal("Notify blocked on a full queue")
	}
	if n.queued > 2 || n.dropped < 3 {
		t.Fatalf("queued %d dropped %d with one worker and a queue of one", n.queued, n.dropped)
	}
	if got := testutil.ToFloat64(d.deliveries.WithLabelValues(core.NotifyFlapping, ResultDropped)); got != float64(n.dropped) {
		t.Fatalf("dropped counter = %v, want %d", got, n.dropped)
	}
}

func TestSetConfigAppliesToNextNotification(t *testing.T) {
	argv, dir := NewRecorder(t)
	d := newTestDispatcher(t, core.NotifyConfig{}, Options{})
	if d.Notify(Notification{Event: core.NotifyFailed, Harness: "a"}) {
		t.Fatal("delivered with notify off")
	}
	d.SetConfig(testConfig(t, argv))
	if !d.Notify(Notification{Event: core.NotifyFailed, Harness: "a"}) {
		t.Fatal("not delivered after the reload turned notify on")
	}
	d.settle(t, dir, 1)
}
