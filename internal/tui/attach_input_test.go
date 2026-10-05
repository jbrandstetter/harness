package tui

// Keystrokes typed (or pasted) in a burst must reach the guest in the order
// they were typed. Bubble Tea runs every Cmd in its own goroutine; with one
// Cmd per keystroke two fast keys race for the connection's write lock and
// arrive swapped, so a prompt typed quickly into an attached claude session
// came out as "integers 1 ot8 ,0 noe per line".

import (
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// lockedAttach records the bytes AttachInput receives, under a lock so that
// a race in the code under test shows up as disorder in the result rather
// than as a data race inside the fake.
type lockedAttach struct {
	fakeAttach
	mu  sync.Mutex
	got []byte
}

func (l *lockedAttach) AttachInput(_ uint32, data []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.got = append(l.got, data...)
	return nil
}

func TestAttachInputKeepsTypedOrder(t *testing.T) {
	m, _ := attachedFake(80, 24)
	rec := &lockedAttach{}
	m.attach = rec

	var want []byte
	var wg sync.WaitGroup
	run := func(cmd tea.Cmd) {
		if cmd == nil {
			return
		}
		wg.Add(1)
		go func() { // the Bubble Tea runtime runs each Cmd on its own goroutine
			defer wg.Done()
			cmd()
		}()
	}
	for i := 0; i < 600; i++ {
		if i%100 == 50 {
			want = append(want, "\x1b[200~PASTE\x1b[201~"...)
			_, cmd := m.Update(tea.PasteMsg{Content: "PASTE"})
			run(cmd)
			continue
		}
		c := byte('a' + i%26)
		want = append(want, c)
		_, cmd := m.Update(runeKey(string(c)))
		run(cmd)
	}
	wg.Wait()

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if string(rec.got) != string(want) {
		for i := 0; i < len(want) && i < len(rec.got); i++ {
			if rec.got[i] != want[i] {
				t.Fatalf("input reordered at byte %d (typed %d bytes, delivered %d):\n got %q\nwant %q", i, len(want), len(rec.got), rec.got[:min(len(rec.got), i+20)], want[:min(len(want), i+20)])
			}
		}
		t.Fatalf("typed %d bytes, delivered %d", len(want), len(rec.got))
	}
}
