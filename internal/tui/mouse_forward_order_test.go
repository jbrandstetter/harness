package tui

// Mouse events forwarded to the guest share the keystroke queue: a wheel turn
// typed between two keys must not overtake or trail them.

import (
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestForwardedMouseKeepsOrderWithKeys(t *testing.T) {
	m, _ := attachedFake(100, 30)
	rec := &lockedAttach{}
	m.attach = rec
	m.att.view.write([]byte("\x1b[?1003h\x1b[?1006h"))

	var want []byte
	var wg sync.WaitGroup
	run := func(cmd tea.Cmd) {
		if cmd == nil {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd()
		}()
	}
	for i := 0; i < 300; i++ {
		if i%3 == 1 {
			want = append(want, "\x1b[<64;50;15M"...)
			wheel := wheelUp()
			wheel.X, wheel.Y = 49, 14
			_, cmd := m.Update(wheel)
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
		t.Fatalf("mouse and keys reordered:\n got %q\nwant %q", rec.got, want)
	}
}
