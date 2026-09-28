// Governing: ADR-0040; SPEC-0026 REQ-1, REQ-6, REQ-8, REQ-9, REQ-11 (write
// path); issue #810.
package tomledit

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

const sample = `# Global harness config.

[daemon]
watch_config = true # keep

[harness.build]
harness = "generic"
args = [
  "make",
  "test", # the important one
]
# trailing comment belongs to build

[harness.deploy]
harness = "generic"
remote = "https://example.invalid/build.git"
`

func TestAddStablePreservesEverything(t *testing.T) {
	e := New([]byte(sample))
	if err := e.AddStable("acme", [][2]any{
		{"remote", "https://forge.example/acme/stable.git"},
		{"public", true},
	}); err != nil {
		t.Fatal(err)
	}
	out := string(e.Bytes())
	if !strings.Contains(out, "[stable.acme]\nremote = \"https://forge.example/acme/stable.git\"\npublic = true\n") {
		t.Errorf("stable table missing or malformed:\n%s", out)
	}
	for _, want := range []string{"# Global harness config.", "watch_config = true # keep", "\"test\", # the important one", "[harness.deploy]"} {
		if !strings.Contains(out, want) {
			t.Errorf("lost %q", want)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "public = true") {
		t.Errorf("stable table not appended at end:\n%s", out)
	}
}

func TestAddStableRefusesDuplicate(t *testing.T) {
	e := New([]byte("[stable.acme]\nremote = \"x\"\n"))
	err := e.AddStable("acme", [][2]any{{"remote", "y"}})
	if !errors.Is(err, ErrDuplicateTable) {
		t.Fatalf("want ErrDuplicateTable, got %v", err)
	}
}

func TestRemoveHarnessHandlesMultiLineArrayAndComments(t *testing.T) {
	e := New([]byte(sample))
	if err := e.RemoveHarness("build"); err != nil {
		t.Fatal(err)
	}
	out := string(e.Bytes())
	if strings.Contains(out, "harness.build") || strings.Contains(out, "the important one") || strings.Contains(out, "trailing comment") {
		t.Errorf("build table not fully removed:\n%s", out)
	}
	if !strings.Contains(out, "[harness.deploy]") || !strings.Contains(out, "[daemon]") {
		t.Errorf("sibling tables lost:\n%s", out)
	}
}

func TestRemoveHarnessBareSpelling(t *testing.T) {
	e := New([]byte("[build]\nharness = \"generic\"\n\n[harness.deploy]\nharness = \"generic\"\n"))
	if err := e.RemoveHarness("build"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(e.Bytes()), "[build]") {
		t.Errorf("bare table not removed:\n%s", e.Bytes())
	}
}

func TestRemoveStableUnknown(t *testing.T) {
	e := New([]byte(sample))
	if err := e.RemoveStable("ghost"); !errors.Is(err, ErrTableNotFound) {
		t.Fatalf("want ErrTableNotFound, got %v", err)
	}
}

func TestSetKeyReplacesInPlace(t *testing.T) {
	e := New([]byte(sample))
	if err := e.SetHarnessKey("deploy", "remote", "https://other.example/x.git"); err != nil {
		t.Fatal(err)
	}
	out := string(e.Bytes())
	if !strings.Contains(out, "remote = \"https://other.example/x.git\"") {
		t.Errorf("value not replaced:\n%s", out)
	}
	if i, j := strings.Index(out, "remote = \"https://other"), strings.LastIndex(out, "[harness.deploy]"); j > i {
		t.Errorf("key not edited in place (moved to end?):\n%s", out)
	}
}

func TestSetKeyAppendsInsideTable(t *testing.T) {
	e := New([]byte(sample))
	if err := e.SetHarnessKey("deploy", "source", "acme/pkg@"+strings.Repeat("0", 40)); err != nil {
		t.Fatal(err)
	}
	out := string(e.Bytes())
	src := "source = \"acme/pkg@" + strings.Repeat("0", 40) + "\""
	if !strings.Contains(out, src) {
		t.Fatalf("source key not written:\n%s", out)
	}
	// The appended key must sit INSIDE the deploy table, not after a later
	// section: deploy is the last table here, so it must be the final line.
	if !strings.HasSuffix(strings.TrimRight(out, "\n"), src) {
		t.Errorf("key appended outside the table:\n%s", out)
	}
	// A second set replaces it in place and does not duplicate.
	if err := e.SetHarnessKey("deploy", "source", "acme/pkg@"+strings.Repeat("1", 40)); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(e.Bytes()), "source ="); n != 1 {
		t.Errorf("source appears %d times, want 1", n)
	}
}

func TestArrayTableRefused(t *testing.T) {
	e := New([]byte("[[stable.acme]]\nremote = \"x\"\n"))
	if err := e.RemoveStable("acme"); !errors.Is(err, ErrArrayTable) {
		t.Fatalf("want ErrArrayTable, got %v", err)
	}
}

func TestDuplicateTableRefused(t *testing.T) {
	e := New([]byte("[harness.build]\nharness = \"crush\"\n\n[harness.build]\nharness = \"crush\"\n"))
	if err := e.SetHarnessKey("build", "model", "opus"); !errors.Is(err, ErrDuplicateTable) {
		t.Fatalf("want ErrDuplicateTable, got %v", err)
	}
}

func TestMultiLineStringBracketIsNotHeader(t *testing.T) {
	// The `"""…[not a header]…"""` body must not end the build table early:
	// the whole table is removed, and a following table survives.
	e := New([]byte("[harness.build]\nharness = \"crush\"\nsystem_prompt_file = \"\"\"\n[not a header]\n\"\"\"\n\n[harness.deploy]\nharness = \"crush\"\n"))
	if err := e.RemoveHarness("build"); err != nil {
		t.Fatal(err)
	}
	out := string(e.Bytes())
	if strings.Contains(out, "[not a header]") || strings.Contains(out, "harness.build") {
		t.Errorf("build table not fully removed:\n%s", out)
	}
	if !strings.Contains(out, "[harness.deploy]") {
		t.Errorf("deploy table lost:\n%s", out)
	}
}

func TestValueEncoding(t *testing.T) {
	e := New([]byte(""))
	if err := e.AddHarness("x", [][2]any{
		{"str", `he said "hi"\ go`},
		{"list", []string{"a", "b c"}},
		{"flag", false},
	}); err != nil {
		t.Fatal(err)
	}
	want := "[harness.x]\nstr = \"he said \\\"hi\\\"\\\\ go\"\nlist = [\"a\", \"b c\"]\nflag = false\n"
	if string(e.Bytes()) != want {
		t.Errorf("got:\n%s\nwant:\n%s", e.Bytes(), want)
	}
	if err := e.AddHarness("y", [][2]any{{"bad", 42}}); err == nil {
		t.Error("unsupported value type accepted")
	}
}

func TestSaveIsAtomicAndKeepsMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "harness.toml")
	if err := os.WriteFile(path, []byte(sample), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.AddStable("acme", [][2]any{{"remote", "https://x"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.Save(path); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if !bytes.Contains(data, []byte("[stable.acme]")) {
		t.Error("saved file lost the new table")
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("left %d files, want 1", len(entries))
	}
}

func TestEditedFileStillParses(t *testing.T) {
	// Whatever the editor does, the result must remain valid TOML with the
	// expected shape — the whole point of a write path the loader trusts.
	e := New([]byte(sample))
	_ = e.AddStable("acme", [][2]any{{"remote", "https://forge.example/acme.git"}, {"public", true}})
	_ = e.SetHarnessKey("build", "model", "opus")
	_ = e.RemoveHarness("deploy")
	_ = e.SetStableKey("acme", "public", false)
	var doc map[string]any
	if _, err := toml.Decode(string(e.Bytes()), &doc); err != nil {
		t.Fatalf("edited file no longer parses: %v\n%s", err, e.Bytes())
	}
	h := doc["harness"].(map[string]any)["build"].(map[string]any)
	if h["model"] != "opus" {
		t.Errorf("build.model = %v", h["model"])
	}
	st := doc["stable"].(map[string]any)["acme"].(map[string]any)
	if st["public"] != false || st["remote"] != "https://forge.example/acme.git" {
		t.Errorf("stable.acme = %v", st)
	}
	if _, exists := doc["harness"].(map[string]any)["deploy"]; exists {
		t.Error("deploy still present")
	}
}
