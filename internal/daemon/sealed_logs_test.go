package daemon

// Sealed Log Reader Tests
//
// A closed run's log is compressed to <id>.log.zst in the background (ADR-0007
// as amended). Every reader the daemon serves a run log from must not notice:
// `harness logs --run`, raw and events alike, and the runs op's has_log and
// log_pruned. These read a run log in each form it can be in on disk — plain
// while open, compressed once sealed, and both at once after a crash between
// the rename and the removal.
//
// Governing: ADR-0007 (as amended for sealed compression); SPEC-0008 REQ
// "Per-Run Logs"; SPEC-0022 REQ-12; GitHub
// https://github.com/stump-wtf/harness/issues/18.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/client"
	"github.com/stump-wtf/harness/internal/sealedlog"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// TestRunLogServedAfterCompression drives the whole path over the wire: a run
// closes, its log is sealed and compressed, and the daemon still serves it,
// still says it has one, and does not call it pruned.
func TestRunLogServedAfterCompression(t *testing.T) {
	dir := t.TempDir()
	td, _, _ := newJobsDaemonWith(t, func(o *supervisor.ManagerOptions) { o.CompressLogs = true },
		scheduledSh("nightly", "echo from-a-compressed-run; exit 0", dir))
	c := td.dial(t, nil)
	if _, err := c.Trigger("nightly"); err != nil {
		t.Fatal(err)
	}
	waitRunsOver(t, c, "nightly", finishedN(1))
	td.mgr.WaitSealed()

	path := td.mgr.RunLogPath("nightly", 1)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("plain run log still on disk after the run closed (err %v)", err)
	}
	if _, err := os.Stat(sealedlog.Compressed(path)); err != nil {
		t.Fatalf("compressed run log missing: %v", err)
	}

	rd, err := c.Runs("nightly", 0)
	if err != nil {
		t.Fatal(err)
	}
	if r := rd.Runs[0]; !r.HasLog || r.LogPruned {
		t.Errorf("runs reports has_log %v, log_pruned %v for a compressed log; want true, false", r.HasLog, r.LogPruned)
	}
	raw, err := c.RunLogs("nightly", 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw.Text, "from-a-compressed-run") || len(raw.Notices) > 0 {
		t.Errorf("raw --run 1 = %q, notices %v", raw.Text, raw.Notices)
	}
	events, err := c.LogEvents("nightly", client.LogOptions{Run: 1, Lines: 100})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(events.Text, "from-a-compressed-run") {
		t.Errorf("events --run 1 text:\n%s", events.Text)
	}
}

// TestReadRunLogTailEitherForm: the tail, and its masking, are the same in
// every form a run log can be in.
func TestReadRunLogTailEitherForm(t *testing.T) {
	remote := logCredFixtures.Replace("<PW_REMOTE>")
	var body strings.Builder
	for i := range 50 {
		body.WriteString("line ")
		body.WriteString(strings.Repeat("x", i))
		body.WriteString("\n")
	}
	body.WriteString("git remote set-url origin " + remote + "\n")
	body.WriteString("last line\n")
	want := redactTail(tailLines([]byte(body.String()), 3))

	for _, form := range []string{"plain", "compressed", "both"} {
		t.Run(form, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "1.log")
			if err := os.WriteFile(path, []byte(body.String()), 0o600); err != nil {
				t.Fatal(err)
			}
			if form != "plain" {
				if err := sealedlog.CompressFile(path); err != nil {
					t.Fatal(err)
				}
			}
			if form == "both" {
				if err := os.WriteFile(path, []byte(body.String()), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, ok := readRunLogTail(path, 3)
			if !ok || got != want {
				t.Errorf("readRunLogTail = %q, %v; want %q", got, ok, want)
			}
			if strings.Contains(got, logPwSecret) {
				t.Errorf("tail leaked a credential: %q", got)
			}
		})
	}
	if _, ok := readRunLogTail(filepath.Join(t.TempDir(), "9.log"), 3); ok {
		t.Error("readRunLogTail reported a log that exists in neither form")
	}
}
