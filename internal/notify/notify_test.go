package notify

// Dispatcher Tests
//
// Governing tests: SPEC-0003 REQ "Operator Notification"; issue #725. Every
// test runs a real hook — a shell script exec'd the way production execs one —
// and reads back what it actually received, so a dispatcher that built the
// right payload and then failed to hand it over cannot pass.
//
// The hook timeout and the waits on a delivery scale with the go test
// deadline (testwait.Budget), and WaitReceived ends on the dispatcher's
// recorded outcome rather than on files turning up, so a loaded machine slows
// these tests down instead of failing them, and a red run names the
// delivery's result.
//
// @joestump-agent 09/28/2026 - Scaled the hook timeout, waited on delivery
// outcomes, and retried the process-group test while its timeout fires before
// the hook has a child to kill: both flaked under `make test race`.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/prometheus/client_golang/prometheus"
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

// WaitReceived waits for d to finish n deliveries, every one of which must
// succeed, and returns what the recorder in dir received.
//
// It waits on the dispatcher's own outcome, not on files turning up. Under a
// concurrent `go test -race ./...` the hook timeout killed the recorder before
// it wrote anything, and a file poll reported that as "hook ran 0 times"; a
// delivery that errors or times out now fails the test at once, with its
// result.
func WaitReceived(t *testing.T, d *Dispatcher, dir string, n int) []Received {
	t.Helper()
	budget := testwait.Budget(t, 10*time.Second)
	deadline := time.Now().Add(budget)
	for {
		done, failed := outcomes(t, d)
		if failed > 0 {
			t.Fatalf("%d of %d finished deliveries failed; last: %+v", failed, done, d.Status().Last)
		}
		if done >= n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d deliveries finished within %s; last: %+v", done, n, budget, d.Status().Last)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The recorder renames its files into place before it exits, so a
	// delivery that succeeded has already left them.
	got := ReadReceived(t, dir)
	if len(got) < n {
		t.Fatalf("%d deliveries succeeded but the hook recorded %d", n, len(got))
	}
	return got
}

// outcomes counts d's deliveries that ran the hook to an outcome, and how
// many of those errored or timed out, from its deliveries counter.
func outcomes(t *testing.T, d *Dispatcher) (done, failed int) {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(d.Collector())
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			n := int(m.GetCounter().GetValue())
			for _, l := range m.GetLabel() {
				if l.GetName() != "result" {
					continue
				}
				switch l.GetValue() {
				case ResultOK:
					done += n
				case ResultError, ResultTimeout:
					done += n
					failed += n
				}
			}
		}
	}
	return done, failed
}

func quietLogger() *log.Logger { return log.New(nopWriter{}) }

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// testConfig is a [notify] table that runs argv. The hook timeout is not what
// these tests measure, so it scales with the go test deadline: a fixed 5s
// killed the recorder mid-delivery under a concurrent `go test -race ./...`.
func testConfig(t *testing.T, argv []string) core.NotifyConfig {
	t.Helper()
	return core.NotifyConfig{
		Command:  argv,
		Events:   slices.Clone(core.DefaultNotifyEvents),
		Timeout:  testwait.Budget(t, 5*time.Second),
		Cooldown: time.Minute,
	}
}

func newTestDispatcher(t *testing.T, cfg core.NotifyConfig, opts Options) *Dispatcher {
	t.Helper()
	if opts.Hostname == nil {
		opts.Hostname = func() (string, error) { return "kitt", nil }
	}
	opts.Logger = quietLogger()
	d := New(cfg, opts)
	t.Cleanup(d.Close)
	return d
}

func TestDeliveryEnvAndStdin(t *testing.T) {
	argv, dir := NewRecorder(t)
	// A stale HARNESS_NOTIFY_* in the daemon's own environment must not
	// reach the hook alongside the real one. os/exec keeps the last of a
	// duplicated key, so a stale key the delivery also sets is overridden
	// whether or not the dispatcher strips it: only one it does not set can
	// show the strip is missing.
	environ := func() []string {
		return append(os.Environ(), "HARNESS_NOTIFY_EVENT=stale", "HARNESS_NOTIFY_STALE=1")
	}
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
	r := WaitReceived(t, d, dir, 1)[0]

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
	if v, ok := r.Env["HARNESS_NOTIFY_STALE"]; ok {
		t.Errorf("the daemon's own HARNESS_NOTIFY_STALE=%s reached the hook", v)
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
	r := WaitReceived(t, d, dir, 1)[0]
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
	got := WaitReceived(t, d, dir, 1)
	time.Sleep(100 * time.Millisecond)
	if got = ReadReceived(t, dir); len(got) != 1 || got[0].Payload.Event != core.NotifyLoopStopped {
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
	WaitReceived(t, d, dir, 4)
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
	WaitReceived(t, d, dir, 3)
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
	WaitReceived(t, d, dir, 3)
}

// The timeout kills the hook's whole process group: a script whose child
// keeps the output pipe open must not hold the delivery (or a worker) past
// it.
//
// That needs the child to exist before the timeout fires, and under a
// concurrent `go test -race ./...` a 1s timeout fired before sh had forked it
// in 5 of 50 runs. Such an attempt killed nothing the test can see, so it
// proves nothing either way: it is retried with twice the timeout, inside the
// go test deadline, rather than failed. An idle machine runs one 1s attempt.
func TestTimeoutKillsProcessGroup(t *testing.T) {
	budget := testwait.Budget(t, 8*time.Second)
	began := time.Now()
	for timeout := time.Second; ; timeout *= 2 {
		if time.Since(began)+timeout > budget {
			t.Fatalf("every timeout up to %s fired before the hook recorded its child's pid: nothing to check the kill against", timeout/2)
		}
		if killsProcessGroup(t, timeout) {
			return
		}
		t.Logf("the %s timeout fired before the hook recorded its child's pid; retrying with %s", timeout, 2*timeout)
	}
}

// killsProcessGroup delivers once to a hook that backgrounds a long sleep,
// records its pid and waits on it, under a timeout of timeout, and checks the
// timeout killed that child. It returns false, having checked nothing, when
// the timeout fired before the hook recorded the pid.
func killsProcessGroup(t *testing.T, timeout time.Duration) bool {
	t.Helper()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := filepath.Join(dir, "slow.sh")
	// The child outlives any timeout this test tries by far, so a delivery
	// the child holds open cannot pass for a slow teardown.
	body := "#!/bin/sh\nsleep 60 &\necho $! > " + pidFile + "\nwait\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, []string{script})
	cfg.Timeout = timeout
	d := newTestDispatcher(t, cfg, Options{})

	start := time.Now()
	del := d.Test(context.Background())
	if del.Result != ResultTimeout {
		t.Fatalf("result = %+v, want timeout", del)
	}
	if took, limit := time.Since(start), timeout+testwait.Budget(t, 3*time.Second); took > limit {
		t.Fatalf("delivery took %s with a %s timeout: the child held it open", took, timeout)
	}
	// No pid means the timeout fired before the hook had a child, which
	// leaves nothing below to check. An empty file is a kill that landed
	// between the redirect and the echo.
	raw, err := os.ReadFile(pidFile)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return false
	}
	deadline := time.Now().Add(testwait.Budget(t, 3*time.Second))
	for alive(pid) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("the hook's child %d outlived the timeout", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := testutil.ToFloat64(d.deliveries.WithLabelValues(core.NotifyTest, ResultTimeout)); got != 1 {
		t.Fatalf("timeout counter = %v, want 1", got)
	}
	return true
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
	dir := t.TempDir()
	script := filepath.Join(dir, "block.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, []string{script})
	cfg.Cooldown = 0
	d := newTestDispatcher(t, cfg, Options{Workers: 1, Queue: 1, ShutdownGrace: 50 * time.Millisecond})
	start := time.Now()
	var queued, dropped int
	for range 5 {
		if d.Notify(Notification{Event: core.NotifyFlapping, Harness: "a"}) {
			queued++
		} else {
			dropped++
		}
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("Notify blocked for %s", took)
	}
	if queued > 2 || dropped < 3 {
		t.Fatalf("queued %d dropped %d with one worker and a queue of one", queued, dropped)
	}
	if got := testutil.ToFloat64(d.deliveries.WithLabelValues(core.NotifyFlapping, ResultDropped)); got != float64(dropped) {
		t.Fatalf("dropped counter = %v, want %d", got, dropped)
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
	WaitReceived(t, d, dir, 1)
}
