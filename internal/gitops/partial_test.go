package gitops

import (
	"os"
	"strings"
	"testing"
)

// A change is several splices. When one of them no longer matched the text,
// the change was reported as refused while its other splices were still
// written, so the file held half an edit nobody was told about.
func TestFailedSpliceUnappliesItsWholeChange(t *testing.T) {
	lines := []string{
		"        cpu: 500m",   // 0: change 0, matches
		"        memory: 1Gi", // 1: change 0, does not match (expects 2Gi)
		"        cpu: 250m",   // 2: change 1, matches
	}
	all := []splice{
		{line: 0, col: 13, oldText: "500m", newText: "200m", owner: 0},
		{line: 1, col: 16, oldText: "2Gi", newText: "512Mi", owner: 0},
		{line: 2, col: 13, oldText: "250m", newText: "100m", owner: 1},
		{line: 0, insertAfter: true, lines: []string{"        # from change 0"}, owner: 0},
	}
	out, failed := applyWhole(append([]string{}, lines...), all)
	if !failed[0] || failed[1] {
		t.Fatalf("failed owners = %v, want only change 0", failed)
	}
	got := strings.Join(out, "\n")
	want := strings.Join([]string{
		"        cpu: 500m",
		"        memory: 1Gi",
		"        cpu: 100m",
	}, "\n")
	if got != want {
		t.Errorf("a refused change left part of its edit behind:\n%s\nwant:\n%s", got, want)
	}
}

// Two changes for one container were both planned against the original bytes.
// Checked one at a time against the edited text, the second used to fail;
// checked up front, both passed and wrote "16MiMi", which still parses.
func TestTwoChangesForOneContainerAreBothRefused(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "api.yaml", deployYAML)

	edits, skipped, err := ApplyTree(dir, []Change{
		change("app", 300, 600, 512<<20, 640<<20),
		change("app", 100, 200, 16<<20, 20<<20),
		change("sidecar", 50, 100, 64<<20, 80<<20),
		change("sidecar", 60, 120, 96<<20, 120<<20),
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != deployYAML || len(edits) != 0 {
		t.Errorf("conflicting changes edited the file:\n%s", got)
	}
	var dup int
	for _, s := range skipped {
		if strings.Contains(s.Why, "more than one recommendation") {
			dup++
		}
	}
	if dup != 4 {
		t.Errorf("want all 4 conflicting changes refused with a reason, got %d: %+v", dup, skipped)
	}
}

func TestOverlappingSplicesRefuseBothOwners(t *testing.T) {
	lines := []string{"        memory: 1Gi"}
	all := []splice{
		{line: 0, col: 16, oldText: "1Gi", newText: "16Mi", owner: 0},
		{line: 0, col: 16, oldText: "1Gi", newText: "20Mi", owner: 1},
	}
	out, failed := applyWhole(append([]string{}, lines...), all)
	if !failed[0] || !failed[1] || out[0] != lines[0] {
		t.Errorf("overlapping splices applied: %q failed=%v", out[0], failed)
	}
}
