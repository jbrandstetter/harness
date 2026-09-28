package settings

// Byte Size Tests
//
// ParseBytes is what stands between "2GiB" in a unit file and the number the
// daemon acts on, so the table covers every accepted unit, the spellings that
// must be refused, and the round trip FormatBytes promises doctor.
//
// Governing: SPEC-0010 REQ "Environment Value Validation".
//
// @joestump-agent 09/28/2026 - Added with bytes.go.

import (
	"math"
	"strings"
	"testing"
)

func TestParseBytes(t *testing.T) {
	good := []struct {
		raw  string
		want int64
	}{
		{"0", 0},
		{"4096", 4096},
		{"4096B", 4096},
		{"1KiB", 1 << 10},
		{"1k", 1 << 10},
		{"1KB", 1 << 10},
		{"1Ki", 1 << 10},
		{"512MiB", 512 << 20},
		{"512mb", 512 << 20},
		{"2GiB", 2 << 30},
		{"2G", 2 << 30},
		{"2gi", 2 << 30},
		{"2 GiB", 2 << 30},
		{"  2GiB  ", 2 << 30},
		{"1TiB", 1 << 40},
		{"1536MiB", 1536 << 20},
		{"8388607TiB", 8388607 << 40},
	}
	for _, tc := range good {
		got, err := ParseBytes(tc.raw)
		if err != nil {
			t.Errorf("ParseBytes(%q) error: %v", tc.raw, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseBytes(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}

	bad := []string{
		"", "   ", "lots", "GiB", "-1", "-1GiB", "+1GiB", "1.5GiB", "2 G i B",
		"2PiB", "2 bytes", "0x10", "8388608TiB", "99999999999999999999",
		"١٢", // Arabic-Indic digits are not a number here
	}
	for _, raw := range bad {
		if got, err := ParseBytes(raw); err == nil {
			t.Errorf("ParseBytes(%q) = %d, want an error", raw, got)
		}
	}
}

// A refusal names the accepted form, so the operator learns what to type.
func TestParseBytesErrorNamesForm(t *testing.T) {
	_, err := ParseBytes("lots")
	if err == nil {
		t.Fatal("ParseBytes(lots) = nil error")
	}
	for _, want := range []string{`"lots"`, "GiB", "MiB"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestFormatBytesRoundTrips(t *testing.T) {
	cases := map[int64]string{
		0:             "0",
		1000:          "1000B",
		1 << 10:       "1KiB",
		512 << 20:     "512MiB",
		1536 << 20:    "1536MiB",
		2 << 30:       "2GiB",
		3 << 40:       "3TiB",
		math.MaxInt64: "9223372036854775807B",
	}
	for n, want := range cases {
		got := FormatBytes(n)
		if got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", n, got, want)
		}
		back, err := ParseBytes(got)
		if err != nil || back != n {
			t.Errorf("ParseBytes(FormatBytes(%d)) = %d, %v; want %d", n, back, err, n)
		}
	}
}

// withBytesSetting registers a KindBytes setting for the duration of a test, so
// the resolver path is covered independently of any real setting using it.
func withBytesSetting(t *testing.T) {
	t.Helper()
	s := Setting{Name: "test-bytes", Env: "HARNESS_TEST_BYTES", FileKey: "daemon.test_bytes", Kind: KindBytes, Desc: "test"}
	saved := Registry
	Registry = append(Registry[:len(Registry):len(Registry)], s)
	t.Cleanup(func() { Registry = saved })
}

// A size resolves through the ladder like every other kind: from the
// environment with its source, from the file as a string or a TOML integer,
// and a bad value fails naming the variable, the value and the accepted form
// (SPEC-0010).
func TestBytesSettingThroughTheLadder(t *testing.T) {
	withBytesSetting(t)

	t.Setenv("HARNESS_TEST_BYTES", "2GiB")
	r := New()
	got, err := r.Bytes("test-bytes")
	if err != nil || got != 2<<30 {
		t.Fatalf("Bytes(test-bytes) under HARNESS_TEST_BYTES=2GiB = %d, %v; want %d", got, err, int64(2<<30))
	}
	res, err := r.Resolve("test-bytes")
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != SourceEnv || res.String() != "2GiB" {
		t.Errorf("resolved %q from %s, want 2GiB from env", res.String(), res.Source)
	}

	t.Setenv("HARNESS_TEST_BYTES", "lots")
	_, err = New().Resolve("test-bytes")
	if err == nil {
		t.Fatal("HARNESS_TEST_BYTES=lots resolved without error")
	}
	for _, want := range []string{"HARNESS_TEST_BYTES", "lots", "GiB"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}

	// A TOML integer is a byte count; a string carries its unit.
	t.Setenv("HARNESS_TEST_BYTES", "")
	for body, want := range map[string]int64{
		"[daemon]\ntest_bytes = 4096\n":       4096,
		"[daemon]\ntest_bytes = \"512MiB\"\n": 512 << 20,
	} {
		r := New()
		if err := r.ReadConfigFile(writeConfig(t, body)); err != nil {
			t.Fatal(err)
		}
		got, err := r.Bytes("test-bytes")
		if err != nil || got != want {
			t.Errorf("file %q: Bytes = %d, %v; want %d", body, got, err, want)
		}
	}

	// Unset with no default is zero, and doctor shows it empty, not "0".
	res, err = New().Resolve("test-bytes")
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != SourceDefault || res.String() != "" {
		t.Errorf("unset: %q from %s, want empty from default", res.String(), res.Source)
	}
}
