package remote

// Governing: SPEC-0002 REQ "Transport Bindings" (scenario "Remote parity"),
// ADR-0004 (Wish data plane hosts the same TUI). stump.wtf/harness#824: an SSH
// client ended up with neither the alt screen nor mouse reporting active while
// the TUI kept declaring both, which deafened the session. These tests pin the
// byte contract a real SSH session must honor, independent of any terminal
// emulator: the enable sequences reach the wire before the first frame, and no
// input path ever emits a disable after them.

import (
	"os"
	"path/filepath"

	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/stump-wtf/harness/internal/attach"
	"github.com/stump-wtf/harness/internal/config"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/daemon"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// outCapture is a concurrency-safe byte sink for a session's stdout.
type outCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *outCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *outCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func (c *outCapture) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Len()
}

// sshTUISession dials the test server, requests a PTY + shell, and streams
// stdout into a capture until the test ends. The mode sequences under test
// arrive in bursts, so a single short Read is not enough — the pipe must be
// drained continuously.
func sshTUISession(t *testing.T, s *Server, signer gossh.Signer) (sess *gossh.Session, stdin io.WriteCloser, out *outCapture) {
	t.Helper()
	conn, err := gossh.Dial("tcp", s.Addr(), &gossh.ClientConfig{
		User:            "joe",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	sess, err = conn.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	stdout, err := sess.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	stdin, err = sess.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	if err := sess.RequestPty("xterm-256color", 40, 120, gossh.TerminalModes{}); err != nil {
		t.Fatalf("request pty: %v", err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}

	out = &outCapture{}
	go func() { _, _ = io.Copy(out, stdout) }()
	return sess, stdin, out
}

// waitForOutputGrowth polls until the capture grows by n bytes or times out,
// so assertions on post-input output do not depend on wall-clock luck.
func waitForOutputGrowth(t *testing.T, out *outCapture, start, n int, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if out.Len() >= start+n {
			return out.Len()
		}
		time.Sleep(25 * time.Millisecond)
	}
	return out.Len()
}

// TestSSHSessionEnablesAltScreenAndMouse pins the #824 enable half: a fresh
// SSH session must put the client terminal into the alt screen and mouse
// reporting (cell motion, SGR encoding) before the TUI becomes usable. Wheel
// scrolling "scrolls the client terminal's native scrollback" exactly when the
// mouse-mode enable never reaches the wire, so assert on bytes, not on a
// terminal emulator.
func TestSSHSessionEnablesAltScreenAndMouse(t *testing.T) {
	socket, _ := bootDaemon(t)
	signer, authLine := clientKey(t)
	s := startServer(t, Options{
		Socket:      socket,
		ConfigPath:  "",
		Version:     "test",
		HostKeyPath: t.TempDir() + "/hostkey",
		Keys:        []core.AuthorizedKey{{Line: authLine}},
	})

	_, _, out := sshTUISession(t, s, signer)
	waitForOutputGrowth(t, out, 0, 2000, 10*time.Second)

	buf := out.String()
	for _, seq := range []struct {
		name string
		esc  string
	}{
		{"alt screen enable", "\x1b[?1049h"},
		{"mouse cell-motion enable", "\x1b[?1002h"},
		{"mouse SGR encoding enable", "\x1b[?1006h"},
	} {
		if !strings.Contains(buf, seq.esc) {
			t.Errorf("client output missing %s (%q); the client terminal would not be in the mode the TUI assumes", seq.name, seq.esc)
		}
	}
}

// TestSSHSessionNeverDisablesModesAfterStartup pins the #824 disable half: the
// TUI owns alt screen and mouse reporting for the whole session. A disable
// emitted mid-session (other than the final teardown on quit) leaves the
// client terminal with modes the TUI's next frames assume are still on.
func TestSSHSessionNeverDisablesModesAfterStartup(t *testing.T) {
	socket, _ := bootDaemon(t)
	signer, authLine := clientKey(t)
	s := startServer(t, Options{
		Socket:      socket,
		ConfigPath:  "",
		Version:     "test",
		HostKeyPath: t.TempDir() + "/hostkey",
		Keys:        []core.AuthorizedKey{{Line: authLine}},
	})

	_, _, out := sshTUISession(t, s, signer)
	waitForOutputGrowth(t, out, 0, 2000, 10*time.Second)

	// Let the TUI settle — repaints, keepalives, whatever it does at rest.
	time.Sleep(1500 * time.Millisecond)
	seen := out.String()
	if strings.Count(seen, "\x1b[?1049h") == 0 {
		t.Fatal("precondition: alt screen never enabled")
	}
	if strings.Contains(seen, "\x1b[?1049l") {
		t.Error("alt screen disabled mid-session; the client would fall out of the TUI's assumed modes")
	}
	for _, esc := range []string{"\x1b[?1002l", "\x1b[?1006l"} {
		if strings.Contains(seen, esc) {
			t.Errorf("mouse reporting disabled mid-session (%q); wheel input would be eaten by the client's native scrollback", esc)
		}
	}
}

// bootDaemonWithConfig is bootDaemon with a caller-supplied harness.toml body,
// so a test can stand up a real attachable harness for the TUI to host.
func bootDaemonWithConfig(t *testing.T, cfg string) string {
	t.Helper()
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "harness.toml")
	if err := os.WriteFile(configPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	c, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sockDir, err := os.MkdirTemp("/tmp", "hnd-remote")
	if err != nil {
		t.Fatalf("sock dir: %v", err)
	}
	socket := filepath.Join(sockDir, "d.sock")

	reg := attach.NewRegistry(1000)
	mgr := supervisor.NewManager(c, supervisor.ManagerOptions{
		StatePath:   filepath.Join(tmp, "state.json"),
		LogDir:      filepath.Join(tmp, "logs"),
		ExtraOutFor: reg.WriterFor,
	})
	reg.SetController(mgr)
	srv := daemon.NewServer(daemon.Options{
		Manager:    mgr,
		Registry:   reg,
		SocketPath: socket,
		ConfigPath: configPath,
		Version:    "test",
	})
	if err := srv.Listen(); err != nil {
		t.Fatalf("daemon listen: %v", err)
	}
	go srv.Serve()
	t.Cleanup(func() {
		srv.Close()
		mgr.Close()
		_ = os.RemoveAll(sockDir)
	})
	return socket
}

// waitForMarker polls until the captured stream contains substr, so view-level
// assertions ride the real frame stream instead of guessing frame boundaries.
func waitForMarker(t *testing.T, out *outCapture, substr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), substr) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in the client byte stream", substr)
}

// TestSSHAttachScrollbackLifecycle pins the #824 repro end to end over a real
// SSH session: attach to a live harness, enter scrollback with a wheel event
// and with the Ctrl-b [ prefix, and prove the session stays live (the help
// overlay still opens) with the client's modes untouched throughout. The one
// legitimate mode change is the shift-passthrough (#49) release and its
// keypress recovery, which must always land as disable-then-enable, never
// leave the mouse off.
func TestSSHAttachScrollbackLifecycle(t *testing.T) {
	socket := bootDaemonWithConfig(t, `[harness.tick]
harness = "generic"
args = ["-c", "while true; do echo tick; sleep 1; done"]
description = "ticking harness for the attach test"
enabled = true
`)
	signer, authLine := clientKey(t)
	s := startServer(t, Options{
		Socket:      socket,
		ConfigPath:  "",
		Version:     "test",
		HostKeyPath: t.TempDir() + "/hostkey",
		Keys:        []core.AuthorizedKey{{Line: authLine}},
	})

	sess, stdin, out := sshTUISession(t, s, signer)
	_ = sess
	waitForMarker(t, out, "tick", 10*time.Second)

	// Attach to the selected (only) harness: Enter.
	_, _ = stdin.Write([]byte("\r"))
	waitForMarker(t, out, "attached: tick", 10*time.Second)
	startup := out.Len()

	// Wheel-up in live attach must enter the TUI's scrollback, not touch any
	// client terminal mode.
	_, _ = stdin.Write([]byte("\x1b[<64;10;10M"))
	waitForMarker(t, out, "-- SCROLLBACK", 10*time.Second)

	// Ctrl-b [ must do the same through the prefix path.
	_, _ = stdin.Write([]byte("q")) // exit scrollback first
	_, _ = stdin.Write([]byte("\x02["))
	waitForMarker(t, out, "-- SCROLLBACK", 10*time.Second)
	_, _ = stdin.Write([]byte("q"))

	// The session must still be alive: the help overlay opens and renders.
	_, _ = stdin.Write([]byte("\x02?"))
	waitForMarker(t, out, "Keymap", 10*time.Second)
	_, _ = stdin.Write([]byte("\x1b")) // close the overlay

	// Shift+click (#49) releases the mouse grab — the one legitimate disable —
	// and any keypress must bring mouse reporting back.
	_, _ = stdin.Write([]byte("\x1b[<4;5;5M"))
	waitForMarker(t, out, "\x1b[?1002l", 10*time.Second)
	_, _ = stdin.Write([]byte("x"))

	// Recovery: poll until a mouse enable lands after the disable, so the
	// assertion rides the wire instead of racing the write.
	recovered := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		seen := out.String()
		if off := strings.LastIndex(seen, "\x1b[?1002l"); off >= 0 {
			if on := strings.LastIndex(seen, "\x1b[?1002h"); on > off {
				recovered = true
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !recovered {
		t.Fatalf("mouse reporting left disabled after recovery keypress (disable at %d, last enable at %d)",
			strings.LastIndex(out.String(), "\x1b[?1002l"), strings.LastIndex(out.String(), "\x1b[?1002h"))
	}

	seen := out.String()
	if idx := strings.Index(seen, "\x1b[?1049l"); idx >= 0 {
		t.Fatalf("alt screen disabled at byte %d, before the session quit", idx)
	}
	// The pre-attach stream (dashboard + attach) must not have disabled
	// anything behind the user's back.
	if pre := seen[:startup]; strings.Contains(pre, "\x1b[?1002l") || strings.Contains(pre, "\x1b[?1006l") {
		t.Error("mouse reporting disabled before any shift-passthrough gesture")
	}
}

// TestSSHMouseReleaseWindowSelfRestores pins the #824 fix: after a
// shift+click releases the mouse grab, the release must end on its own —
// with no keypress sent — so a wheel that follows finds mouse reporting back
// on instead of scrolling the client terminal's native scrollback.
func TestSSHMouseReleaseWindowSelfRestores(t *testing.T) {
	socket := bootDaemonWithConfig(t, `[harness.tick]
harness = "generic"
args = ["-c", "while true; do echo tick; sleep 1; done"]
description = "ticking harness for the attach test"
enabled = true
`)
	signer, authLine := clientKey(t)
	s := startServer(t, Options{
		Socket:      socket,
		ConfigPath:  "",
		Version:     "test",
		HostKeyPath: t.TempDir() + "/hostkey",
		Keys:        []core.AuthorizedKey{{Line: authLine}},
	})

	sess, stdin, out := sshTUISession(t, s, signer)
	_ = sess
	waitForMarker(t, out, "tick", 10*time.Second)

	_, _ = stdin.Write([]byte("\r"))
	waitForMarker(t, out, "attached: tick", 10*time.Second)

	// Shift+click releases the grab; nothing else is sent from here.
	_, _ = stdin.Write([]byte("\x1b[<4;5;5M"))
	waitForMarker(t, out, "\x1b[?1002l", 10*time.Second)
	off := strings.LastIndex(out.String(), "\x1b[?1002l")

	// No keypress: the window must close by itself and re-enable mouse
	// reporting on the wire.
	restored := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if on := strings.LastIndex(out.String(), "\x1b[?1002h"); on > off {
			restored = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !restored {
		t.Fatalf("mouse grab never self-restored within 5s (disable at byte %d, no later enable)", off)
	}
	if strings.Contains(out.String(), "\x1b[?1049l") {
		t.Error("alt screen disabled mid-session")
	}
}
