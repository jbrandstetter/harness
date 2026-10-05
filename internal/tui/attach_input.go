package tui

import (
	"sync"

	tea "charm.land/bubbletea/v2"
)

// attachInputQueue delivers an attached session's input in the order it was
// typed. Bubble Tea runs every Cmd in its own goroutine, so a Cmd per
// keystroke lets two keys typed close together race for the connection's
// write lock and reach the guest swapped ("to" arrives as "ot"). Update is
// single-threaded, so frames are queued there in order, and at most one Cmd
// at a time drains the queue.
type attachInputQueue struct {
	mu      sync.Mutex
	pending []attachInput
	running bool
}

type attachInput struct {
	conn attachConn
	sid  uint32
	data []byte
}

// sendAttachInput queues data for the attached session and returns the Cmd
// that delivers it, or nil when an already-running drain will.
func (m *Model) sendAttachInput(sid uint32, data []byte) tea.Cmd {
	if m.inq == nil {
		m.inq = &attachInputQueue{}
	}
	return m.inq.push(attachInput{conn: m.attach, sid: sid, data: data})
}

func (q *attachInputQueue) push(in attachInput) tea.Cmd {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending = append(q.pending, in)
	if q.running {
		return nil
	}
	q.running = true
	return q.drain
}

func (q *attachInputQueue) drain() tea.Msg {
	for {
		q.mu.Lock()
		if len(q.pending) == 0 {
			q.running = false
			q.mu.Unlock()
			return nil
		}
		in := q.pending[0]
		q.pending = q.pending[1:]
		q.mu.Unlock()
		_ = in.conn.AttachInput(in.sid, in.data)
	}
}
