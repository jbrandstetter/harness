//go:build linux || darwin

package daemon

// Governing: issue #835, ask 1 — where the platform gives it, an enabled-
// intent change from a socket verb carries the peer's uid/pid, so the journal
// line says which user and which process flipped the intent.

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestPeerCredentialsUnix(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "hnp-peer")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	ln, err := net.Listen("unix", filepath.Join(dir, "s.sock"))
	if err != nil {
		t.Skip("unix sockets unavailable: ", err)
	}
	defer ln.Close()

	peer := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			peer <- ""
			return
		}
		peer <- peerCredentials(c)
		_ = c.Close()
	}()

	c, err := net.Dial("unix", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	want := "uid=" + strconv.Itoa(os.Getuid()) + " pid=" + strconv.Itoa(os.Getpid())
	if got := <-peer; got != want {
		t.Fatalf("peer = %q, want %q", got, want)
	}
}

func TestPeerCredentialsNonUnix(t *testing.T) {
	if got := peerCredentials(nil); got != "" {
		t.Fatalf("peer = %q, want empty for a non-socket connection", got)
	}
}
