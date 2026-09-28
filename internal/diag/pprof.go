package diag

// Profiling Listener
//
// [daemon] pprof_addr serves net/http/pprof, so the next memory problem is a
// heap profile away instead of an afternoon of guessing. The daemon that
// reached 12 GB had no profiler, and the leak was found by reading code
// (GitHub https://github.com/stump-wtf/harness/issues/18).
//
// It is off unless an address is set, and it binds loopback only: 127.0.0.0/8,
// ::1 or localhost, by the same IsLoopback that decides whether the metrics
// listener needs a token. Anything else is refused, and there is no token
// option to unlock it, unlike /metrics (SPEC-0013 REQ-1). A heap profile names
// every allocation site in the daemon, a goroutine dump carries argument
// values, /debug/pprof/cmdline is the daemon's argv, and /debug/pprof/profile
// burns CPU on request. None of that belongs on a network. A remote operator
// tunnels to it: ssh -L 6060:127.0.0.1:6060 host.
//
// "localhost" is resolved by the system at bind time, so the address actually
// bound is checked again after net.Listen, and a listener that landed off
// loopback is closed and refused.
//
// The handlers are mounted on a mux of their own. Importing net/http/pprof
// also registers them on http.DefaultServeMux, which is harmless only because
// no server in this binary serves DefaultServeMux; TestMetricsListenerHasNoPprof
// in internal/metrics pins that for /metrics.
//
// Governing: SPEC-0013 REQ-8 (opt-in pprof, loopback only), REQ-1 (the
// metrics listener's bind rules, which this follows); ADR-0020.
//
// @joestump-agent 09/28/2026 - Added for GitHub
// https://github.com/stump-wtf/harness/issues/18.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"strconv"
	"strings"
	"time"
)

// ErrPprofNotLoopback reports a pprof_addr that would bind off loopback.
var ErrPprofNotLoopback = errors.New("pprof binds loopback only (127.0.0.1, ::1 or localhost)")

// IsLoopback reports whether host names only the loopback interface. An empty
// host (":6060") binds every interface and is not loopback. This is the one
// definition of loopback for every daemon listener: metrics.IsLoopback, the
// webhook listener's warning and doctor all use it.
func IsLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i] // an IPv6 zone does not change the address class
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// CheckPprofAddr accepts "" (off) or host:port with a numeric port and a
// loopback host. Port 0 is accepted and binds an ephemeral port.
func CheckPprofAddr(addr string) error {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not host:port: %w", addr, err)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("%q has no valid port", addr)
	}
	if !IsLoopback(host) {
		return fmt.Errorf("%q: %w; reach it from elsewhere through an SSH tunnel", addr, ErrPprofNotLoopback)
	}
	return nil
}

// PprofServer is a running profiling listener.
type PprofServer struct {
	srv *http.Server
	ln  net.Listener
}

// ListenPprof checks addr, binds it synchronously (a nil error means the port
// is held), and serves /debug/pprof/ on a private mux. An empty addr is an
// error; the caller decides whether pprof is on.
func ListenPprof(addr string) (*PprofServer, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil, errors.New("pprof listener is off")
	}
	if err := CheckPprofAddr(addr); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	if tcp, ok := ln.Addr().(*net.TCPAddr); !ok || !tcp.IP.IsLoopback() {
		_ = ln.Close()
		return nil, fmt.Errorf("%q bound %s: %w", addr, ln.Addr(), ErrPprofNotLoopback)
	}
	s := &PprofServer{
		// No write timeout: /debug/pprof/profile and /trace stream for as
		// many seconds as the request asks.
		srv: &http.Server{Handler: PprofMux(), ReadHeaderTimeout: 10 * time.Second},
		ln:  ln,
	}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

// PprofMux is the profiling handler set on a fresh mux. pprof.Index serves
// the named profiles (heap, goroutine, allocs, block, mutex, threadcreate)
// under /debug/pprof/<name>.
func PprofMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}

// Addr is the bound address, with the real port when addr asked for :0.
func (s *PprofServer) Addr() string { return s.ln.Addr().String() }

// Shutdown stops accepting and waits for requests in flight, up to ctx. A
// profile still streaming when ctx ends is cut off.
func (s *PprofServer) Shutdown(ctx context.Context) error {
	err := s.srv.Shutdown(ctx)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		_ = s.srv.Close()
	}
	return err
}
