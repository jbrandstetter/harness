package tui

// Coverage for issue #825: attached mode renders a claude-code harness's
// stream-json through the same PeekFormatter the dashboard's preview uses,
// ^b f flips back to the byte-faithful mirror, and the status bar names the
// mode. The transformation itself is adapter-side
// (internal/adapter/streamjson.go); these tests prove the attach wiring: who
// gets a formatter, that its output reaches the emulator, and that the
// toggle moves both ways.

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// attachModelWithAdapter is attachedFake with the first harness's adapter
// named, at a width that keeps one stream-json line un-wrapped so substring
// assertions can see it whole.
func attachModelWithAdapter(t *testing.T, adapterName string) (*Model, *fakeAttach) {
	t.Helper()
	m, fa := attachedFake(200, 50)
	m.harnesses[0].Adapter = adapterName
	return m, fa
}

// toolCall builds one stream-json line carrying a tool call with a
// human-written description — the line a human is reading attach for. Each
// call needs its own description: the emulator keeps what it has already
// painted, so a toggle test must be able to tell a new frame's bytes from an
// older frame's.
func toolCall(desc string) []byte {
	return []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"git push","description":"` + desc + `"}}]}}` + "\n")
}

func TestAttachFormatsClaudeCodeStreamJSON(t *testing.T) {
	m, _ := attachModelWithAdapter(t, "claude-code")
	drain(m.attachTo(m.harnesses[0], 0))
	if m.att.fmt == nil {
		t.Fatal("attaching to a claude-code harness opened without a formatter")
	}

	m.Update(attachDataMsg{sessionID: m.att.sessionID, data: toolCall("Push branch to GitHub")})
	got := m.att.view.render()
	if !strings.Contains(got, "Push branch to GitHub") {
		t.Errorf("the tool call's description did not reach the attached view:\n%s", got)
	}
	if strings.Contains(got, `"type":"tool_use"`) {
		t.Errorf("attached view shows raw stream-json:\n%s", got)
	}
}

func TestAttachOtherAdaptersStayByteFaithful(t *testing.T) {
	for _, name := range []string{"", "crush", "generic"} {
		m, _ := attachModelWithAdapter(t, name)
		drain(m.attachTo(m.harnesses[0], 0))
		if m.att.fmt != nil {
			t.Fatalf("%q attach got a formatter; only claude-code opts in", name)
		}
		m.Update(attachDataMsg{sessionID: m.att.sessionID, data: toolCall("raw json for a non-claude backend")})
		if got := m.att.view.render(); !strings.Contains(got, `"type":"tool_use"`) {
			t.Errorf("%q attach no longer mirrors the guest's bytes:\n%s", name, got)
		}
	}
}

// TestAttachRawToggle: ^b f moves between the readable view and the raw
// mirror and back. The toggle takes effect on subsequent frames — the guest's
// screen already holds whatever it painted, and a one-shot keeps writing, so
// each phase asserts on its own new frame's bytes.
func TestAttachRawToggle(t *testing.T) {
	m, _ := attachModelWithAdapter(t, "claude-code")
	drain(m.attachTo(m.harnesses[0], 0))

	m.Update(ctrlKey('b'))
	m.Update(specialKey('f'))
	if !m.att.raw {
		t.Fatal("^b f did not flip the attach to the raw mirror")
	}
	if got := ansi.Strip(m.viewStatusBar()); !strings.Contains(got, "RAW") {
		t.Errorf("status bar does not name the raw mode:\n%s", got)
	}
	m.Update(attachDataMsg{sessionID: m.att.sessionID, data: toolCall("Second call, raw")})
	if got := m.att.view.render(); !strings.Contains(got, `"description":"Second call, raw"`) {
		t.Errorf("raw mode still transformed the guest's bytes:\n%s", got)
	}

	m.Update(ctrlKey('b'))
	m.Update(specialKey('f'))
	if m.att.raw {
		t.Fatal("a second ^b f did not flip back to the readable view")
	}
	m.Update(attachDataMsg{sessionID: m.att.sessionID, data: toolCall("Third call, readable")})
	got := m.att.view.render()
	if !strings.Contains(got, "Third call, readable") {
		t.Errorf("readable mode did not format the new frame:\n%s", got)
	}
	if strings.Contains(got, `"description":"Third call, readable"`) {
		t.Errorf("readable mode passed raw JSON through:\n%s", got)
	}
}

// TestAttachRawToggleIsANoOpWithoutAFormatter: a backend with no formatter
// has only ever had the mirror; ^b f must not flip it into a "raw" mode that
// means nothing (and must not badge it).
func TestAttachRawToggleIsANoOpWithoutAFormatter(t *testing.T) {
	m, _ := attachModelWithAdapter(t, "generic")
	drain(m.attachTo(m.harnesses[0], 0))

	m.Update(ctrlKey('b'))
	m.Update(specialKey('f'))
	if m.att.raw {
		t.Fatal("a formatterless attach was flipped to raw")
	}
	if got := ansi.Strip(m.viewStatusBar()); strings.Contains(got, "RAW") {
		t.Errorf("status bar badges a raw mode that does not exist:\n%s", got)
	}
}
