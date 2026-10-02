// Prune: remove every entry under the content-addressed store that no
// source in the GLOBAL harness.toml references. Project files are never
// read here (SPEC-0026 REQ-9): a pin only a project references is removed,
// and the next `harness up` for that project fails with REQ-7's
// missing-pin error rather than silently refetching it.
//
// Governing: ADR-0044 (agent package stables), SPEC-0026 REQ-9 (uninstall
// and prune), Error Handling Standards.
//
// @joestump-agent 10/02/2026 - Added for harness#813.
package agentpkg

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Prune removes every store entry no source references and returns what it
// removed, as "<stable>/<package>@<sha>" strings. Read-only pins are
// chmodded writable again before removal — that is the one place the
// visible immutability is undone, deliberately, by the operator's own
// prune.
func Prune(referenced map[Source]bool) ([]string, error) {
	root := InstalledRoot()
	var removed []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil // nothing installed yet
			}
			return err
		}
		if !d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) != 3 || !shaRe.MatchString(parts[2]) {
			return nil // stable/, package/ parents and temp dirs are not pins
		}
		s := Source{Stable: parts[0], Package: parts[1], SHA: parts[2]}
		if referenced[s] {
			return nil
		}
		// Restore write permission on the tree before removing it: that is
		// the one place the visible immutability is undone, deliberately,
		// by the operator's own prune.
		if err := chmodTreeWritable(path); err != nil {
			return err
		}
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("agentpkg: prune %s: %w", s, err)
		}
		removed = append(removed, s.String())
		// The pin's install record, if any, goes with the pin.
		_ = os.Remove(path + ".record.toml")
		// Leave no empty stable/ or package/ directories behind.
		os.Remove(filepath.Dir(path))
		os.Remove(filepath.Dir(filepath.Dir(path)))
		return nil
	})
	if err != nil {
		return removed, fmt.Errorf("agentpkg: prune %s: %w", root, err)
	}
	return removed, nil
}

// ChmodTreeWritable restores write permission over a materialized (read-only)
// tree, so a caller can remove or clean it up.
func ChmodTreeWritable(root string) error {
	return chmodTreeWritable(root)
}

func chmodTreeWritable(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		mode := os.FileMode(0o755)
		if !d.IsDir() {
			mode = 0o644
		}
		return os.Chmod(path, mode)
	})
}
