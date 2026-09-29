package main

// Config Load Warnings Reach The Operator
//
// core.Config.Warnings are a load's non-fatal findings. They used to be
// appended and then read by nothing but tests: the daemon never logged them
// and doctor never showed them, while doctor's triggers row re-derived one of
// them on its own. These tests drive what the daemon and doctor actually
// build (#315): the real binary logs them at start and on every reload, and
// doctor's config row shows each one exactly once, with the triggers row
// judging only what the load leaves to the bind.
//
// Every positive case has a control that shows the check can stay quiet, so
// none passes merely because every load warns (CLAUDE.md "A zero").
//
// Governing: SPEC-0014 REQ "Credential Resolution" (a readable env_file SHALL
// produce a warning, and doctor SHALL flag it), REQ "Webhook Listener" (a
// non-loopback bind without TLS SHALL log a warning at startup, and doctor
// SHALL flag it).
//
// @joestump 09/29/2026 - Introduced when cfg.Warnings got its consumers.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/cliui"
	"github.com/stump-wtf/harness/internal/config"
)

// laxWebhookConfig writes a harness.toml declaring [webhook.gh], whose
// env_file is written at mode, plus any extra TOML, and returns the config
// path and the env_file path.
func laxWebhookConfig(t *testing.T, dir string, mode os.FileMode, extra string) (cfgPath, envPath string) {
	t.Helper()
	envPath = filepath.Join(dir, "hook.env")
	if err := os.WriteFile(envPath, []byte("TOK=hook-token\n"), mode); err != nil {
		t.Fatal(err)
	}
	// WriteFile honours the umask; chmod pins the mode under test.
	if err := os.Chmod(envPath, mode); err != nil {
		t.Fatal(err)
	}
	cfgPath = filepath.Join(dir, "harness.toml")
	body := extra + `
[webhook.gh]
verify = "bearer"
env_file = "hook.env"
secret = "${TOK}"
`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, envPath
}

// laxEnvFileLog is the daemon's log line for [webhook.gh]'s readable env_file.
const laxEnvFileLog = "config: [webhook.gh]: env_file"

// warnLines counts the WARN lines in out that contain marker.
func warnLines(out, marker string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, marker) && strings.Contains(line, "WARN") {
			n++
		}
	}
	return n
}

// TestDaemonLogsConfigWarningsAtStartAndReload runs the real binary, so it
// covers what runDaemon wires rather than a copy of it: the start-up load
// logs the warning once, a reload over the socket logs it again, and a reload
// after the operator fixes the mode logs nothing. The daemon writes its log
// straight to a file before it answers, so each count is read after the
// reload reply with no settling delay.
func TestDaemonLogsConfigWarningsAtStartAndReload(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped under -short")
	}
	bin := buildHarnessBinary(t)
	env, _ := isolatedEnv(t)
	// No watcher: only the reloads below may run.
	env = append(env, "HARNESS_WATCH_CONFIG=false")

	dir := shortSockDir(t)
	cfgPath, envPath := laxWebhookConfig(t, dir, 0o644, "")
	socket := filepath.Join(dir, "h.sock")
	d := startDaemonProc(t, bin, env, "daemon", "start", "--socket", socket, "--config", cfgPath, "--log-level", "info")
	d.waitReady(t, bin, env, socket)

	if n := warnLines(d.output(), laxEnvFileLog); n != 1 {
		t.Fatalf("start-up logged the readable env_file %d times, want 1\n%s", n, d.output())
	}
	if !strings.Contains(d.output(), envPath) {
		t.Errorf("the warning does not name %s\n%s", envPath, d.output())
	}
	if strings.Contains(d.output(), "hook-token") {
		t.Fatal("the daemon log carries the env_file's secret value")
	}

	reload := func() {
		t.Helper()
		if out, errOut, code := runCLIStdout(t, bin, env, "--socket", socket, "reload"); code != 0 {
			t.Fatalf("reload exit %d: %s%s", code, out, errOut)
		}
	}
	reload()
	if n := warnLines(d.output(), laxEnvFileLog); n != 2 {
		t.Fatalf("after a reload the warning was logged %d times in all, want 2\n%s", n, d.output())
	}

	if err := os.Chmod(envPath, 0o600); err != nil {
		t.Fatal(err)
	}
	reload()
	if n := warnLines(d.output(), laxEnvFileLog); n != 2 {
		t.Errorf("a reload after chmod 600 logged the warning again (%d in all, want still 2)\n%s", n, d.output())
	}
}

// TestConfigWarningsReloadHook drives the reload reaction the daemon hands
// startDaemonScheduler through a real Manager's ReloadFromFile, in process,
// so the reload half fails fast and under -short.
func TestConfigWarningsReloadHook(t *testing.T) {
	cfgPath, envPath := laxWebhookConfig(t, t.TempDir(), 0o644, "")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	mgr := newWiringManager(t, cfg)

	var buf syncBuffer
	prev := log.Default()
	log.SetDefault(log.New(&buf))
	t.Cleanup(func() { log.SetDefault(prev) })

	clock := &stubClock{now: time.Now(), ticks: make(chan time.Time)}
	sched := startDaemonScheduler(mgr, cfg, clock, configWarningsReload(mgr), telemetryReloadWarning(mgr, cfg.Telemetry))
	t.Cleanup(sched.Close)

	if err := mgr.ReloadFromFile(cfgPath); err != nil {
		t.Fatal(err)
	}
	if n := warnLines(buf.String(), laxEnvFileLog); n != 1 {
		t.Fatalf("reload logged the readable env_file %d times, want 1; log:\n%s", n, buf.String())
	}

	if err := os.Chmod(envPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mgr.ReloadFromFile(cfgPath); err != nil {
		t.Fatal(err)
	}
	if n := warnLines(buf.String(), laxEnvFileLog); n != 1 {
		t.Errorf("a reload after chmod 600 logged it again (%d in all, want still 1); log:\n%s", n, buf.String())
	}
}

// doctorRows runs doctor --json against cfgPath with no daemon and returns
// every row that has a detail, keyed by its JSON field.
func doctorRows(t *testing.T, cfgPath string) map[string]checkResult {
	t.Helper()
	cliui.SetJSON(true)
	t.Cleanup(func() { cliui.SetJSON(false) })
	socket := filepath.Join(shortSockDir(t), "none.sock")
	out, _ := captureStdout(t, func() error {
		runDoctor(verbOpts{configPath: cfgPath, socket: socket})
		return nil
	})
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("doctor --json: %v\n%s", err, out)
	}
	rows := map[string]checkResult{}
	for k, v := range raw {
		var r checkResult
		if json.Unmarshal(v, &r) == nil && r.Detail != "" {
			rows[k] = r
		}
	}
	return rows
}

// rowsMentioning names the rows whose detail contains s.
func rowsMentioning(rows map[string]checkResult, s string) []string {
	var out []string
	for k, r := range rows {
		if strings.Contains(r.Detail, s) {
			out = append(out, k)
		}
	}
	return out
}

// TestDoctorShowsEachConfigWarningOnce runs doctor itself on a config with
// both conditions that used to be judged twice: a readable env_file (the
// load's finding, on the config row) and a non-loopback listener with no TLS
// (the bind's, on the triggers row). Each must appear on exactly one row.
func TestDoctorShowsEachConfigWarningOnce(t *testing.T) {
	cfgPath, envPath := laxWebhookConfig(t, t.TempDir(), 0o644, "[server]\nwebhook_listen = \"0.0.0.0:9080\"\n")
	rows := doctorRows(t, cfgPath)

	cfgRow := rows["config"]
	if cfgRow.Status != cliui.LevelWarn.String() {
		t.Errorf("config row status = %q, want %q: %+v", cfgRow.Status, cliui.LevelWarn.String(), cfgRow)
	}
	if !strings.Contains(cfgRow.Detail, "[webhook.gh]") || !strings.Contains(cfgRow.Detail, envPath) {
		t.Errorf("config row detail = %q, want the load warning naming [webhook.gh] and %s", cfgRow.Detail, envPath)
	}
	if got := rowsMentioning(rows, "group or other"); len(got) != 1 || got[0] != "config" {
		t.Errorf("rows reporting the readable env_file = %v, want only [config]", got)
	}
	if got := rowsMentioning(rows, "0.0.0.0:9080"); len(got) != 1 || got[0] != "triggers" {
		t.Errorf("rows reporting the cleartext listener = %v, want only [triggers]", got)
	}
	if strings.Contains(cfgRow.Detail, "hook-token") || strings.Contains(rows["triggers"].Detail, "hook-token") {
		t.Fatal("doctor printed the env_file's secret value")
	}
}

// TestDoctorConfigRowQuietWithoutWarnings is the control: the same source
// with a 0600 env_file loads with no warnings, and the config row passes.
func TestDoctorConfigRowQuietWithoutWarnings(t *testing.T) {
	cfgPath, _ := laxWebhookConfig(t, t.TempDir(), 0o600, "")
	rows := doctorRows(t, cfgPath)
	if r := rows["config"]; r.Status != cliui.LevelSuccess.String() || strings.Contains(r.Detail, "warning") {
		t.Errorf("config row = %+v, want a pass with no warnings", r)
	}
}

// TestWebhookListenerWarnsAtBind is the other half of the de-duplication: the
// load no longer warns about a non-loopback listener, so the daemon's bind
// has to. It drives beginDaemonWebhooks/serve, the daemon's own wiring, with
// the override the flag and HARNESS_WEBHOOK_LISTEN supply — the case a load
// warning could never see.
func TestWebhookListenerWarnsAtBind(t *testing.T) {
	const warning = "webhook listener is not on loopback and serves no TLS"
	for _, tc := range []struct {
		addr string
		want int
	}{
		{"127.0.0.1:0", 0},
		{"0.0.0.0:0", 1},
		// An empty host binds every interface: the case the removed load
		// warning special-cased, now diag.IsLoopback's.
		{":0", 1},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			mgr := newWiringManager(t, webhookWiringConfig(filepath.Join(t.TempDir(), "fields"), true))
			sources := startDaemonSources(mgr, nil)
			t.Cleanup(sources.Close)

			var buf syncBuffer
			prev := log.Default()
			log.SetDefault(log.New(&buf))
			t.Cleanup(func() { log.SetDefault(prev) })

			webhooks := beginDaemonWebhooks(mgr, sources, tc.addr)
			webhooks.serve()
			t.Cleanup(webhooks.shutdown)
			if webhooks.addr() == "" {
				t.Fatalf("the listener did not bind %s; log:\n%s", tc.addr, buf.String())
			}
			if n := strings.Count(buf.String(), warning); n != tc.want {
				t.Errorf("bind of %s logged the cleartext warning %d times, want %d; log:\n%s", tc.addr, n, tc.want, buf.String())
			}
		})
	}
}
