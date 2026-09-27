package tui

// Chatroom History Fetch
//
// The chatroom's watcher only sees the last fifteen minutes of the stores on
// this machine (watcher.go). Entering the chatroom therefore also asks the
// daemon for every harness's latest run — the structured `logs` reply — and
// merges it into the stream (chatroom/history.go), then keeps asking while
// the view is open, so a harness whose sessions this machine cannot read (a
// remote daemon, a crush instance with its own data directory) still speaks.
//
// Governing: ADR-0015 (chatroom TUI), SPEC-0009 REQ "Multi-Harness Event
// Aggregation", SPEC-0002 REQ "Control Operations" ("logs").
//
// @joestump-agent 09/27/2026 - Added: the chatroom opened empty.

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/stump-wtf/harness/internal/client"
	"github.com/stump-wtf/harness/internal/protocol"
)

const (
	// chatHistoryLines is the per-harness budget: the newest entries of each
	// latest run, as `harness logs` defaults to.
	chatHistoryLines = 200
	// chatHistoryPoll is how often an open chatroom re-asks. Each reply
	// parses that harness's transcripts on the daemon, so this is slower than
	// the preview's poll of one harness.
	chatHistoryPoll = 10 * time.Second
)

// chatHistoryMsg is one harness's structured `logs` reply for the chatroom.
type chatHistoryMsg struct {
	name, adapter string
	data          protocol.LogsData
	err           error
}

// chatHistoryCmd asks the daemon for every harness's latest run, one request
// per harness so each merges as it arrives. Nothing is asked while an earlier
// round is still being answered.
func (m *Model) chatHistoryCmd() tea.Cmd {
	if m.ctrl == nil || m.chatHistPending > 0 {
		return nil
	}
	var cmds []tea.Cmd
	for _, h := range m.harnesses {
		if h.Adapter == "generic" {
			continue // no transcript to read (ADR-0033)
		}
		ctrl, name, adapter := m.ctrl, h.Name, h.Adapter
		cmds = append(cmds, func() tea.Msg {
			ld, err := ctrl.LogEvents(name, client.LogOptions{Lines: chatHistoryLines})
			return chatHistoryMsg{name: name, adapter: adapter, data: ld, err: err}
		})
	}
	if len(cmds) == 0 {
		return nil
	}
	m.chatHistPending, m.chatHistAt = len(cmds), time.Now()
	return tea.Batch(cmds...)
}

// onChatHistory merges one reply. A failed request is skipped, not surfaced:
// the live stream still runs, and the next round asks again.
func (m *Model) onChatHistory(msg chatHistoryMsg) {
	if m.chatHistPending > 0 {
		m.chatHistPending--
	}
	if msg.err != nil {
		return
	}
	m.ensureChatroom()
	m.chatroom.AddHistory(msg.name, msg.adapter, msg.data)
}

// chatHistoryDue reports whether an open chatroom should re-ask.
func (m *Model) chatHistoryDue() bool {
	return m.mode == modeChatroom && m.chatHistPending == 0 && time.Since(m.chatHistAt) >= chatHistoryPoll
}
