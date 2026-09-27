package tui

// Coverage for the chatroom's history fetch: entering the chatroom asks the
// daemon for every harness's latest run and merges it, so the view is not
// empty just because no agent acted in the watcher's fifteen-minute window.

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/stump-wtf/harness/internal/protocol"
)

func TestEnterChatroomMergesEveryHarnessHistory(t *testing.T) {
	m, _ := peekModel()
	m.harnesses[4].Adapter = "generic" // no transcript: never asked
	fc := m.ctrl.(*fakeController)
	fc.events = func(name string) protocol.LogsData {
		ld := oneShotActivity(name)
		for i := range ld.Entries {
			ld.Entries[i].Session = "sess-" + name
			ld.Entries[i].ID = "sess-" + name + "/tool/" + ld.Entries[i].ID
		}
		return ld
	}

	pump(m, m.enterChatroom())

	if len(fc.eventCalls) != len(m.harnesses)-1 {
		t.Fatalf("history fetches = %v, want every harness but the generic one", fc.eventCalls)
	}
	for _, c := range fc.eventCalls {
		if c == m.harnesses[4].Name {
			t.Errorf("fetched history for a generic harness")
		}
	}
	view := ansi.Strip(m.chatroom.View())
	for _, want := range []string{"@" + m.harnesses[0].Name, "@" + m.harnesses[1].Name, "exec", "go vet ./..."} {
		if !strings.Contains(view, want) {
			t.Errorf("chatroom lacks %q:\n%s", want, view)
		}
	}
}

// TestChatroomHistoryPollsWhileOpen: an open chatroom keeps asking — a harness
// whose sessions this machine cannot read has no other way in — but never
// while a round is still being answered, and never once the view is closed.
func TestChatroomHistoryPollsWhileOpen(t *testing.T) {
	m, _ := peekModel()
	cmd := m.enterChatroom()
	if cmd == nil || m.chatHistPending == 0 {
		t.Fatal("entering the chatroom asked for no history")
	}
	if m.chatHistoryDue() {
		t.Fatal("a round still being answered is due again")
	}
	pump(m, cmd)
	if m.chatHistPending != 0 {
		t.Fatalf("pending = %d after every reply arrived", m.chatHistPending)
	}
	m.chatHistAt = time.Now().Add(-chatHistoryPoll)
	if !m.chatHistoryDue() {
		t.Fatal("an open chatroom is not due after the poll interval")
	}
	m.exitChatroom()
	if m.chatHistoryDue() {
		t.Fatal("a closed chatroom is still polling")
	}
}
