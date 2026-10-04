package core

// Hold Reason Set Tests
//
// The set's wire form is a contract (SPEC-0021 REQ-16: hold_reasons), so the
// canonical order and the empty case are pinned here, along with the
// set algebra the supervisor's hold and release lean on.
//
// Governing: SPEC-0021 REQ-14, REQ-16.
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#468.

import (
	"slices"
	"testing"
)

func TestHoldSetCanonicalOrder(t *testing.T) {
	// Built backwards: the order out is the admission order (SPEC-0021
	// REQ-4), never insertion order.
	s := HoldSetOf(HoldBudget, HoldQuota, HoldHours)
	if got, want := s.Strings(), []string{"hours", "quota", "budget"}; !slices.Equal(got, want) {
		t.Errorf("Strings() = %q, want %q", got, want)
	}
	if got, want := s.Reasons(), []HoldReason{HoldHours, HoldQuota, HoldBudget}; !slices.Equal(got, want) {
		t.Errorf("Reasons() = %v, want %v", got, want)
	}
	if got := s.String(); got != "hours,quota,budget" {
		t.Errorf("String() = %q", got)
	}
}

func TestHoldSetEmpty(t *testing.T) {
	var s HoldSet
	if !s.Empty() || s.Strings() != nil || s.Reasons() != nil || s.String() != "none" {
		t.Errorf("zero set: empty=%v strings=%q reasons=%v string=%q", s.Empty(), s.Strings(), s.Reasons(), s.String())
	}
	// The zero reason is no reason: adding it leaves the set empty, which is
	// what keeps a forgotten field from holding a harness forever.
	if !s.With(0).Empty() || HoldSetOf(0, 0).Has(0) {
		t.Error("the zero HoldReason must never be held")
	}
}

func TestHoldSetWithWithout(t *testing.T) {
	s := HoldSetOf(HoldHours).With(HoldQuota)
	if !s.Has(HoldHours) || !s.Has(HoldQuota) || s.Has(HoldBudget) {
		t.Fatalf("set = %s, want hours,quota", s)
	}
	if s.With(HoldHours) != s {
		t.Error("adding a reason already held changed the set")
	}
	s = s.Without(HoldQuota)
	if s != HoldSetOf(HoldHours) {
		t.Errorf("after removing quota: %s, want hours", s)
	}
	if s.Without(HoldBudget) != s {
		t.Error("removing a reason not held changed the set")
	}
	if !s.Without(HoldHours).Empty() {
		t.Error("removing the last reason left the set non-empty")
	}
}

func TestHoldReasonNames(t *testing.T) {
	for r, want := range map[HoldReason]string{HoldHours: "hours", HoldQuota: "quota", HoldBudget: "budget", 0: "", 99: ""} {
		if got := r.String(); got != want {
			t.Errorf("HoldReason(%d).String() = %q, want %q", r, got, want)
		}
		if r.Valid() != (want != "") {
			t.Errorf("HoldReason(%d).Valid() = %v", r, r.Valid())
		}
	}
}
