package agentpkg

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnifiedDiffBasics(t *testing.T) {
	cases := []struct {
		name     string
		old, new []string
		want     string
		notWant  string
	}{
		{
			name: "insert", old: []string{"a", "b"}, new: []string{"a", "x", "b"},
			want: "+x", notWant: "-a",
		},
		{
			name: "delete", old: []string{"a", "x", "b"}, new: []string{"a", "b"},
			want: "-x", notWant: "+b",
		},
		{
			name: "replace", old: []string{"a", "x", "c"}, new: []string{"a", "y", "c"},
			want: "-x", notWant: "=a",
		},
		{
			name: "identical", old: []string{"a", "b"}, new: []string{"a", "b"},
			want: "", notWant: "@@",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := UnifiedDiff("a/f", "b/f", tc.old, tc.new)
			if tc.want == "" {
				if strings.Contains(got, "@@") {
					t.Fatalf("identical input must produce no hunks:\n%s", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("diff must contain %q:\n%s", tc.want, got)
			}
			if tc.notWant != "" && strings.Contains(got, tc.notWant) {
				t.Fatalf("diff must not contain %q:\n%s", tc.notWant, got)
			}
			if !strings.HasPrefix(got, "--- a/f\n+++ b/f\n") {
				t.Fatalf("diff must carry the file labels:\n%s", got)
			}
		})
	}
}

// A hunk far from the file start reports the right line numbers, and two
// separate changes produce two hunks.
func TestUnifiedDiffHunkHeaders(t *testing.T) {
	old := make([]string, 20)
	nw := append([]string(nil), old...)
	nw[5] = "changed"
	nw[15] = "also changed"
	got := UnifiedDiff("a/f", "b/f", old, nw)
	hunks := strings.Count(got, "@@ -")
	if hunks != 2 {
		t.Fatalf("two distant changes must produce two hunks:\n%s", got)
	}
	if !strings.Contains(got, "+changed") || !strings.Contains(got, "+also changed") {
		t.Fatalf("both changes must appear:\n%s", got)
	}
}

// Files further apart than the bounded Myers search render as one
// whole-file replacement hunk rather than failing.
func TestUnifiedDiffWholeFileFallback(t *testing.T) {
	old := make([]string, maxEditDistance)
	for i := range old {
		old[i] = fmt.Sprintf("old-%d", i)
	}
	nw := make([]string, maxEditDistance)
	for i := range nw {
		nw[i] = fmt.Sprintf("new-%d", i)
	}
	got := UnifiedDiff("a/f", "b/f", old, nw)
	if !strings.Contains(got, "@@") {
		t.Fatalf("the fallback must still render a hunk:\n%s", got[:200])
	}
}

func TestManifestChanges(t *testing.T) {
	old := &Manifest{Package: PackageMeta{Name: "p", Version: "1.0.0", Description: "d"}}
	nw := &Manifest{Package: PackageMeta{Name: "p", Version: "1.1.0"}}
	lines := ManifestChanges(old, nw)
	if len(lines) != 2 {
		t.Fatalf("want version and description changes, got %v", lines)
	}
	if !strings.Contains(lines[0], `package.version: 1.0.0 -> 1.1.0`) {
		t.Fatalf("version change must render key by key: %v", lines)
	}
	if !strings.Contains(strings.Join(lines, "\n"), `package.description: d -> ""`) {
		t.Fatalf("a cleared value must render as an empty string: %v", lines)
	}
}

func writeDiffDir(t *testing.T, files map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// PinDiff: manifest changes first, unified diffs for text files under the
// 64 KiB limit, byte counts for larger or binary files, and added or
// removed files (REQ-8).
func TestPinDiff(t *testing.T) {
	smallOld := "line one\nline two\nline three\n"
	smallNew := "line one\nline TWO\nline three\nline four\n"

	bigOld := strings.Repeat("x", 70*1024)
	bigNew := strings.Repeat("y", 71*1024)

	binOld := []byte{0x00, 0x01, 0x02, 0x03}
	binNew := []byte{0x00, 0x01, 0x09}

	oldDir := writeDiffDir(t, map[string][]byte{
		"package.toml": []byte(agentPkgManifest),
		"notes.md":     []byte(smallOld),
		"big.txt":      []byte(bigOld),
		"image.bin":    binOld,
		"gone.txt":     []byte("bye\n"),
	})
	newDir := writeDiffDir(t, map[string][]byte{
		"package.toml": []byte(agentPkgManifest),
		"notes.md":     []byte(smallNew),
		"big.txt":      []byte(bigNew),
		"image.bin":    binNew,
		"fresh.txt":    []byte("hi\n"),
	})

	lines, err := PinDiff(oldDir, newDir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"removed gone.txt",
		"added fresh.txt",
		"-line two",
		"+line TWO",
		"+line four",
		"changed big.txt (71680 -> 72704 bytes; not shown as text)",
		"changed image.bin (4 -> 3 bytes; not shown as text)",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("diff must contain %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "package.toml") {
		t.Errorf("an unchanged file must not appear in the diff:\n%s", joined)
	}
}

const agentPkgManifest = `[package]
name = "p"

[harness]
harness = "claude-code"
`
