package adapter

// Pi And OMP Adapter Tests
//
// Governing tests: SPEC-0017 REQ-13 "Pi And OMP Adapters" (scenarios "OMP
// one-shot argv", "auto_accept is inert", "OMP sessions are attributed") and
// REQ-4 "Transcript Binding"; design.md "pi and omp: one implementation, two
// registry entries".
//
// The session fixtures in testdata/ are built from the CLIs' source, not
// recorded from a running binary (neither was available): pi-session.jsonl
// follows Pi v0.87.1's session format as agent-trace's Pi reader parses it,
// and omp-session.jsonl is the same body under OMP v18.3.0's layout — the
// fixed-width 256-byte title slot (session-title-slot.ts) before the version-3
// header (session-entries.ts). Replace them with recorded sessions when a
// binary is to hand.

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stump-wtf/agent-trace/tail"

	"github.com/stump-wtf/harness/internal/core"
)

// The golden: the pinned print-mode argv, model mapped to --model, prompt last.
func TestPiFamilyPromptArgv(t *testing.T) {
	cases := []struct {
		name     string
		adapter  *PiFamily
		opts     core.AgentOpts
		wantExe  string
		wantArgs []string
	}{
		{"omp with model", OMP, core.AgentOpts{Model: "openrouter/z-ai/glm-5.3-flash"}, "omp",
			[]string{"--print", "--model", "openrouter/z-ai/glm-5.3-flash", "fix the build"}},
		{"omp without model", OMP, core.AgentOpts{}, "omp",
			[]string{"--print", "fix the build"}},
		{"pi with model", Pi, core.AgentOpts{Model: "anthropic/claude-opus-5"}, "pi",
			[]string{"--print", "--model", "anthropic/claude-opus-5", "fix the build"}},
		// auto_accept, max_turns and quiet are accepted and emit nothing:
		// no permission prompts, no turn budget, and --print is already quiet.
		{"inert knobs", Pi, core.AgentOpts{AutoAccept: true, MaxTurns: 40, Quiet: true}, "pi",
			[]string{"--print", "fix the build"}},
		{"quiet off changes nothing", OMP, core.AgentOpts{Quiet: false}, "omp",
			[]string{"--print", "fix the build"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exe, args := c.adapter.PromptCommand("fix the build", c.opts)
			if exe != c.wantExe || !slices.Equal(args, c.wantArgs) {
				t.Fatalf("PromptCommand = %q %q, want %q %q", exe, args, c.wantExe, c.wantArgs)
			}
		})
	}
}

func TestPiFamilyRegistered(t *testing.T) {
	r := NewRegistry()
	for _, want := range []struct{ name, exe string }{{"pi", "pi"}, {"omp", "omp"}} {
		a, err := r.Get(want.name)
		if err != nil {
			t.Fatalf("Get(%q): %v", want.name, err)
		}
		if a.Name() != want.name || a.Executable() != want.exe {
			t.Errorf("%s: Name/Executable = %q/%q", want.name, a.Name(), a.Executable())
		}
		if got := r.Resolve(core.Harness{Adapter: want.name}); got != a {
			t.Errorf("Resolve(%s) = %s", want.name, got.Name())
		}
	}
}

// The session root follows PI_CODING_AGENT_DIR (both CLIs read it) and
// otherwise each CLI's own default under HOME.
func TestPiFamilySessionRoot(t *testing.T) {
	home := "/home/op"
	cases := []struct {
		fam  *PiFamily
		env  map[string]string
		want string
	}{
		{Pi, nil, "/home/op/.pi/agent/sessions"},
		{OMP, nil, "/home/op/.omp/agent/sessions"},
		{Pi, map[string]string{PiAgentDirEnv: "/srv/pi-agent"}, "/srv/pi-agent/sessions"},
		{OMP, map[string]string{PiAgentDirEnv: "/srv/omp-agent"}, "/srv/omp-agent/sessions"},
		{Pi, map[string]string{PiAgentDirEnv: "~/agents/pi"}, "/home/op/agents/pi/sessions"},
		{Pi, map[string]string{PiAgentDirEnv: ""}, "/home/op/.pi/agent/sessions"},
	}
	for _, c := range cases {
		if got := c.fam.SessionRoot(c.env, home); got != c.want {
			t.Errorf("%s SessionRoot(%v) = %q, want %q", c.fam.Name(), c.env, got, c.want)
		}
	}
}

// Pi and OMP are both observed through agent-trace's reader, at their own
// session roots; the OMP adapter labels its sessions HarnessOMP.
func TestPiFamilyTrajectory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(PiAgentDirEnv, "")
	ta, ok := Pi.TailAdapter().(*tail.PiAdapter)
	if !ok || ta.Dir != Pi.TrajectoryDir("") || ta.Dir == "" || ta.OMP {
		t.Fatalf("pi TailAdapter = %#v, want a PiAdapter (OMP unset) at %q", Pi.TailAdapter(), Pi.TrajectoryDir(""))
	}
	oa, ok := OMP.TailAdapter().(*tail.PiAdapter)
	if !ok || oa.Dir != OMP.TrajectoryDir("") || oa.Dir == "" || !oa.OMP {
		t.Fatalf("omp TailAdapter = %#v, want a PiAdapter with OMP set at %q", OMP.TailAdapter(), OMP.TrajectoryDir(""))
	}
}

// The Pi fixture is the control: the pinned reader parses it, tool call and
// all, so a failure on the OMP fixture below is about OMP's layout.
func TestPiSessionFixtureParses(t *testing.T) {
	events, _, meta, err := tail.PiAdapter{}.Parse(context.Background(), filepath.Join("testdata", "pi-session.jsonl"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(events) != 1 || events[0].Tool != "read" {
		t.Fatalf("events = %+v, want the one read call", events)
	}
	if meta.Cwd != "/work/pi" || meta.Model != "z-ai/glm-5.3-flash" {
		t.Errorf("meta cwd/model = %q/%q", meta.Cwd, meta.Model)
	}
}

// TestOMPSessionsAreReadByOMPReader pins that agent-trace reads OMP session
// files as-is: every OMP session opens with a 256-byte {"type":"title"} slot
// line before the {"type":"session"} header, and the reader (>= v0.6.0)
// accepts it. If agent-trace regresses and stops parsing the fixture, this
// fails — flip PiFamily.observed for OMP back to false and update the omp
// docs until it reads them again.
func TestOMPSessionsAreReadByOMPReader(t *testing.T) {
	path := filepath.Join("testdata", "omp-session.jsonl")
	events, _, meta, err := tail.PiAdapter{OMP: true}.Parse(context.Background(), path)
	if err != nil || len(events) != 1 || meta.Cwd != "/work/omp" {
		t.Fatalf("OMP reader: events %d, cwd %q, err %v; want 1 event at /work/omp", len(events), meta.Cwd, err)
	}
	if !OMP.Observed() {
		t.Fatal("agent-trace reads OMP sessions, but OMP is marked unobserved")
	}
}

// A command harness bound with `transcripts` takes the bound adapter's
// trajectory surface; spawn's Resolve is untouched.
func TestTrajectoryAdapterFollowsTranscripts(t *testing.T) {
	r := NewRegistry()
	cases := []struct {
		h    core.Harness
		want string
	}{
		{core.Harness{Adapter: "command", Argv: []string{"claude"}, Transcripts: "claude-code"}, "claude-code"},
		{core.Harness{Adapter: "command", Argv: []string{"pi"}, Transcripts: "pi"}, "pi"},
		{core.Harness{Adapter: "command", Argv: []string{"x"}}, "command"},
		{core.Harness{Adapter: "crush"}, "crush"},
		{core.Harness{Adapter: "omp"}, "omp"},
	}
	for _, c := range cases {
		if got := r.TrajectoryAdapter(c.h).Name(); got != c.want {
			t.Errorf("TrajectoryAdapter(%s/%q) = %s, want %s", c.h.Adapter, c.h.Transcripts, got, c.want)
		}
		if got := r.Resolve(c.h).Name(); got != c.h.Adapter {
			t.Errorf("Resolve(%s/%q) = %s; a binding must never change what spawns", c.h.Adapter, c.h.Transcripts, got)
		}
	}
	bound := core.Harness{Adapter: "command", Argv: []string{"claude"}, Transcripts: "claude-code"}
	if r.TrajectoryAdapter(bound).TailAdapter() == nil {
		t.Error("a claude-code-bound command harness has no tail adapter")
	}
}
