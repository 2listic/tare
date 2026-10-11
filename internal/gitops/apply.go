package gitops

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Edit is one file the plan would change.
type Edit struct {
	Path    string
	Before  string
	After   string
	Applied []Change
}

// ApplyTree rewrites the manifests in dir to carry the planned changes.
//
// It edits through yaml.Node rather than unmarshalling into structs, because a
// round trip through Go types discards comments, key order and every field the
// struct does not know about. A diff that reorders a colleague's file and drops
// their comments will not be merged however correct its numbers are.
//
// Nothing is written when dryRun is set; the Edit values carry before and after
// either way, so a caller can render a diff without touching the working tree.
func ApplyTree(dir string, changes []Change, dryRun bool) ([]Edit, []Skipped, error) {
	if len(changes) == 0 {
		return nil, nil, nil
	}
	// Index by the identity a manifest actually carries. Namespace is matched
	// loosely on purpose: a repo's manifests often omit it and let the apply
	// target decide, so an empty namespace in the file matches anything.
	byKey := map[objectKey][]Change{}
	for _, c := range changes {
		k := objectKey{c.Kind, c.Name}
		byKey[k] = append(byKey[k], c)
	}

	matched := map[string]bool{}
	refused := map[string]string{} // found, deliberately not edited, and why
	var edits []Edit

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "vendor" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if ext := strings.ToLower(filepath.Ext(path)); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil // unreadable file is not a reason to abandon the run
		}
		// A Helm template is not YAML until it has been rendered. Editing one
		// as if it were produces a corrupt file, so they are left alone and
		// reported rather than mangled.
		if bytes.Contains(raw, []byte("{{")) {
			return nil
		}

		out, applied, no, changed, perr := editRaw(raw, lookupIn(byKey, path))
		for _, r := range no {
			refused[r.c.id()] = r.why
		}
		if perr == nil && changed && d.Type()&fs.ModeSymlink != 0 {
			perr = fmt.Errorf("is a symlink, and tare does not write through one to a file that may be outside the repository")
		}
		if perr != nil {
			for _, c := range applied {
				refused[c.id()] = fmt.Sprintf("%s %v", path, perr)
			}
			return nil
		}
		for _, c := range applied {
			matched[c.id()] = true
		}
		if !changed {
			return nil
		}
		edits = append(edits, Edit{Path: path, Before: string(raw), After: out, Applied: applied})
		if !dryRun {
			if werr := writeFileAtomic(path, []byte(out)); werr != nil {
				return fmt.Errorf("writing %s: %w", path, werr)
			}
		}
		return nil
	})
	if err != nil {
		return edits, nil, err
	}

	return edits, unmatched(changes, matched, refused), nil
}

// objectKey is the identity a manifest carries.
type objectKey struct{ kind, name string }

// lookupIn finds the changes for one object in the file at path.
func lookupIn(byKey map[objectKey][]Change, path string) func(kind, name, ns string) ([]Change, []refusal) {
	return func(kind, name, ns string) ([]Change, []refusal) {
		var want []Change
		for _, c := range byKey[objectKey{kind, name}] {
			// An empty namespace in the file matches anything: repos often
			// omit it and let the apply target decide.
			if ns == "" || ns == c.Namespace {
				want = append(want, c)
			}
		}
		if ns != "" {
			return want, nil
		}
		// A manifest with no namespace is usually a Kustomize base serving
		// several. Two namespaces' numbers for one container cannot both be
		// written into it: the first used to win, and both were reported as
		// applied.
		nss := map[string]map[string]bool{}
		for _, c := range want {
			if nss[c.Container] == nil {
				nss[c.Container] = map[string]bool{}
			}
			nss[c.Container][c.Namespace] = true
		}
		var keep []Change
		var no []refusal
		for _, c := range want {
			if len(nss[c.Container]) > 1 {
				no = append(no, refusal{c, fmt.Sprintf(
					"%s sets no namespace and tare has different recommendations for this container in %d namespaces; "+
						"writing one would change the others, so use --kustomize to patch each overlay", path, len(nss[c.Container]))})
				continue
			}
			keep = append(keep, c)
		}
		return keep, no
	}
}

// unmatched is every change the tree did not apply, and why. Anything the tree
// did not contain has to be said out loud: silence would let a reviewer believe
// the diff covers the whole report.
func unmatched(changes []Change, matched map[string]bool, refused map[string]string) []Skipped {
	var missing []Skipped
	for _, c := range changes {
		id := c.id()
		if matched[id] {
			continue
		}
		if why, ok := refused[id]; ok {
			missing = append(missing, Skipped{Workload: c.workload(), Container: c.Container, Why: why})
			continue
		}
		missing = append(missing, Skipped{
			Workload:  c.workload(),
			Container: c.Container,
			Why:       "no plain manifest for it was found in the repository (it may be generated by a Helm chart, which tare does not edit)",
		})
	}
	return missing
}

// writeFileAtomic replaces path through a temporary file and a rename, so a
// failure part way leaves the old manifest rather than a truncated one. It
// writes the file itself, never through a symlink to somewhere outside the
// repository, and keeps the file's mode.
func writeFileAtomic(path string, data []byte) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to write through a symlink")
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tare-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), fi.Mode().Perm()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
