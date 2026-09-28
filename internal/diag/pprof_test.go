package diag

// Profiling Listener Tests
//
// Two properties: an address off loopback is refused before anything binds,
// and a loopback listener serves real profiles and nothing else. Each serving
// test reads a profile body, not just a status, since a 200 from the wrong
// handler would pass a status check.
//
// Governing: SPEC-0013 REQ-8.
//
// @joestump-agent 09/28/2026 - Added with pprof.go.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestIsLoopback(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1": true, "127.1.2.3": true, "::1": true, "localhost": true, "LOCALHOST": true, "fe80::1%lo0": false,
		"": false, "0.0.0.0": false, "::": false, "10.0.0.1": false, "example.com": false, "::ffff:10.0.0.1": false,
	} {
		if got := IsLoopback(host); got != want {
			t.Errorf("IsLoopback(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestCheckPprofAddr(t *testing.T) {
	for _, addr := range []string{"", "  ", "127.0.0.1:6060", "[::1]:6060", "localhost:6060", "127.0.0.1:0"} {
		if err := CheckPprofAddr(addr); err != nil {
			t.Errorf("CheckPprofAddr(%q) = %v, want nil", addr, err)
		}
	}
	for _, addr := range []string{"0.0.0.0:6060", ":6060", "10.0.0.1:6060", "[::]:6060", "example.com:6060", "192.168.1.5:6060"} {
		err := CheckPprofAddr(addr)
		if !errors.Is(err, ErrPprofNotLoopback) {
			t.Errorf("CheckPprofAddr(%q) = %v, want ErrPprofNotLoopback", addr, err)
		}
	}
	for _, addr := range []string{"127.0.0.1", "127.0.0.1:abc", "127.0.0.1:70000", "6060"} {
		if err := CheckPprofAddr(addr); err == nil {
			t.Errorf("CheckPprofAddr(%q) = nil, want a syntax error", addr)
		}
	}
}

// Off loopback, ListenPprof refuses without binding, whatever port is asked.
func TestListenPprofRefusesNonLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", ":0", "[::]:0"} {
		s, err := ListenPprof(addr)
		if s != nil {
			_ = s.Shutdown(context.Background())
			t.Fatalf("ListenPprof(%q) bound %s", addr, s.Addr())
		}
		if !errors.Is(err, ErrPprofNotLoopback) {
			t.Errorf("ListenPprof(%q) = %v, want ErrPprofNotLoopback", addr, err)
		}
	}
	if _, err := ListenPprof(""); err == nil {
		t.Error("ListenPprof(\"\") = nil error; an empty address is off, not a listener")
	}
}

func fetch(t *testing.T, url string) (int, string) {
	t.Helper()
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func TestListenPprofServesOnLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:0", "localhost:0"} {
		t.Run(addr, func(t *testing.T) {
			s, err := ListenPprof(addr)
			if err != nil {
				t.Fatalf("ListenPprof(%q): %v", addr, err)
			}
			base := "http://" + s.Addr()

			// The heap profile an operator runs `go tool pprof` against; debug=1
			// is its text form, so the body can be checked.
			code, body := fetch(t, base+"/debug/pprof/heap?debug=1")
			if code != http.StatusOK || !strings.Contains(body, "heap profile") {
				t.Errorf("GET /debug/pprof/heap?debug=1 = %d, body without a heap profile: %.200q", code, body)
			}
			code, body = fetch(t, base+"/debug/pprof/goroutine?debug=1")
			if code != http.StatusOK || !strings.Contains(body, "goroutine profile") {
				t.Errorf("GET /debug/pprof/goroutine?debug=1 = %d: %.200q", code, body)
			}
			code, body = fetch(t, base+"/debug/pprof/")
			if code != http.StatusOK || !strings.Contains(body, "heap") {
				t.Errorf("GET /debug/pprof/ = %d: %.200q", code, body)
			}
			// Nothing but pprof on this listener.
			if code, _ := fetch(t, base+"/metrics"); code != http.StatusNotFound {
				t.Errorf("GET /metrics on the pprof listener = %d, want 404", code)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := s.Shutdown(ctx); err != nil {
				t.Fatalf("Shutdown: %v", err)
			}
			c := &http.Client{Timeout: 2 * time.Second}
			if resp, err := c.Get(base + "/debug/pprof/"); err == nil {
				resp.Body.Close()
				t.Error("pprof listener still answering after Shutdown")
			}
		})
	}
}
