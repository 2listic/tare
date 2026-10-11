package gitops

import (
	"os"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"
)

// The shapes below are each a way a real manifest differs from deployYAML. Every
// one of them used to either panic mid-tree or write something other than what
// was reported, so each test checks the file on disk, not just the return value.

const emptyResources = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  namespace: prod
spec:
  template:
    spec:
      containers:
        - name: app
          image: api:1.4
          resources: RESOURCES
        - name: sidecar
          image: proxy:0.9
`

// `resources: {}` is what most chart scaffolds render, and it panicked on
// res.Content[0] partway through the tree, leaving earlier files rewritten.
func TestEmptyResourcesGainABlock(t *testing.T) {
	for _, empty := range []string{"{}", "", "null", "~", "{}   # filled in later"} {
		t.Run(empty, func(t *testing.T) {
			dir := t.TempDir()
			body := strings.Replace(emptyResources, " RESOURCES", " "+empty, 1)
			if empty == "" {
				body = strings.Replace(emptyResources, " RESOURCES", "", 1)
			}
			p := write(t, dir, "api.yaml", body)
			edits, missing, err := ApplyTree(dir, []Change{change("app", 300, 600, 512<<20, 640<<20)}, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(edits) != 1 || len(missing) != 0 {
				t.Fatalf("edits=%d missing=%+v", len(edits), missing)
			}
			out, _ := os.ReadFile(p)
			got := resourcesOf(t, string(out), "app")
			if got["requests"]["cpu"] != "300m" || got["limits"]["memory"] != "640Mi" {
				t.Errorf("resources = %v\n%s", got, out)
			}
		})
	}
}

// An alias cannot be edited where it is used without editing what it points
// at, so it is refused, and the refusal says why rather than blaming Helm.
func TestAliasedResourcesAreRefusedWithAReason(t *testing.T) {
	body := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  namespace: prod
spec:
  template:
    spec:
      containers:
        - name: app
          resources: &shared
            requests:
              cpu: 2
        - name: other
          resources: *shared
`
	for _, ctr := range []string{"app", "other"} {
		t.Run(ctr, func(t *testing.T) {
			dir := t.TempDir()
			p := write(t, dir, "api.yaml", body)
			_, missing, err := ApplyTree(dir, []Change{change(ctr, 300, 600, 512<<20, 640<<20)}, false)
			if err != nil {
				t.Fatal(err)
			}
			if out, _ := os.ReadFile(p); string(out) != body {
				t.Errorf("a resources block shared through an anchor was edited:\n%s", out)
			}
			if len(missing) != 1 || !strings.Contains(missing[0].Why, "anchor") {
				t.Errorf("the refusal must name the anchor: %+v", missing)
			}
		})
	}
}

// An edit to a shared scalar through its anchor changes every container that
// references it.
func TestAnchoredSectionIsNotEditedThroughAnotherContainer(t *testing.T) {
	body := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  template:
    spec:
      containers:
        - name: b
          resources:
            requests: &q
              cpu: 100m
        - name: c
          resources:
            requests: *q
`
	dir := t.TempDir()
	p := write(t, dir, "api.yaml", body)
	c := Change{Namespace: "prod", Kind: "Deployment", Name: "api", Container: "b", CPURequest: ptr(500)}
	_, missing, err := ApplyTree(dir, []Change{c}, false)
	if err != nil {
		t.Fatal(err)
	}
	if out, _ := os.ReadFile(p); string(out) != body {
		t.Errorf("editing b rewrote the anchor c also reads:\n%s", out)
	}
	if len(missing) != 1 || !strings.Contains(missing[0].Why, "anchor") {
		t.Errorf("the refusal must name the anchor: %+v", missing)
	}
}

// A container built from a merge key inherits its resources; adding a block
// beside the merge key would silently override the inherited limits.
func TestMergeKeyContainerIsRefused(t *testing.T) {
	body := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  template:
    spec:
      containers:
        - &base
          name: app
          resources:
            limits:
              memory: 1Gi
        - <<: *base
          name: worker
`
	dir := t.TempDir()
	p := write(t, dir, "api.yaml", body)
	c := Change{Namespace: "prod", Kind: "Deployment", Name: "api", Container: "worker", CPURequest: ptr(500)}
	_, missing, err := ApplyTree(dir, []Change{c}, false)
	if err != nil {
		t.Fatal(err)
	}
	if out, _ := os.ReadFile(p); string(out) != body {
		t.Errorf("a merge-key container was edited:\n%s", out)
	}
	if len(missing) != 1 || !strings.Contains(missing[0].Why, "merge key") {
		t.Errorf("the refusal must name the merge key: %+v", missing)
	}
}

// A kustomize base usually has no namespace and serves several. Recommendations
// for the same container in two namespaces cannot both be written into it, and
// writing either one changes the other namespace's pods too.
func TestNamespacelessManifestWithConflictingNamespacesIsRefused(t *testing.T) {
	body := strings.Replace(deployYAML, "  namespace: prod\n", "", 1)
	dir := t.TempDir()
	p := write(t, dir, "base/api.yaml", body)
	prod := change("app", 300, 600, 512<<20, 640<<20)
	staging := change("app", 100, 200, 128<<20, 160<<20)
	staging.Namespace = "staging"
	edits, missing, err := ApplyTree(dir, []Change{prod, staging}, false)
	if err != nil {
		t.Fatal(err)
	}
	if out, _ := os.ReadFile(p); string(out) != body {
		t.Errorf("one namespace's numbers were written into a shared base:\n%s", out)
	}
	if len(edits) != 0 || len(missing) != 2 {
		t.Fatalf("edits=%d missing=%+v", len(edits), missing)
	}
	for _, m := range missing {
		if !strings.Contains(m.Why, "namespace") {
			t.Errorf("the refusal must explain the namespace conflict: %+v", m)
		}
	}
}

// A file checked out on Windows must keep its line endings: the old token
// included the \r, so the replacement dropped it and the file came out mixed.
func TestCRLFIsPreserved(t *testing.T) {
	body := strings.ReplaceAll(deployYAML, "\n", "\r\n")
	dir := t.TempDir()
	p := write(t, dir, "api.yaml", body)
	if _, _, err := ApplyTree(dir, []Change{change("app", 300, 600, 512<<20, 640<<20), change("sidecar", 50, 100, 64<<20, 80<<20)}, false); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(p)
	if n, crlf := strings.Count(string(out), "\n"), strings.Count(string(out), "\r\n"); n != crlf {
		t.Errorf("%d of %d line endings are CRLF:\n%q", crlf, n, out)
	}
	if got := resourcesOf(t, string(out), "app"); got["requests"]["cpu"] != "300m" {
		t.Errorf("app requests = %v", got)
	}
	if got := resourcesOf(t, string(out), "sidecar"); got["limits"]["cpu"] != "100m" {
		t.Errorf("sidecar limits = %v", got)
	}
}

// yaml.v3 counts columns in characters; slicing a line by them in bytes landed
// the edit two bytes early after an "é", wrote "cpu:500m" - a bare key that
// still parses - and reported the change as applied.
func TestNonASCIIBeforeTheValueOnTheSameLine(t *testing.T) {
	body := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  template:
    spec:
      containers:
        - name: app
          resources: {requests: {note: "é", cpu: 100m}}
`
	dir := t.TempDir()
	p := write(t, dir, "api.yaml", body)
	c := Change{Namespace: "prod", Kind: "Deployment", Name: "api", Container: "app", CPURequest: ptr(500)}
	if _, _, err := ApplyTree(dir, []Change{c}, false); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(p)
	if got := resourcesOf(t, string(out), "app"); got["requests"]["cpu"] != "500m" {
		t.Errorf("requests = %v\n%s", got, out)
	}
}

// Deployment/web and StatefulSet/web are different objects and must not share
// a patch file; a container name that YAML reads as a number must stay a string.
func TestKustomizePatchesAreDistinctAndQuoted(t *testing.T) {
	a := Change{Namespace: "prod", Kind: "Deployment", Name: "web", Container: "123", CPURequest: ptr(100)}
	b := Change{Namespace: "prod", Kind: "StatefulSet", Name: "web", Container: "app", CPURequest: ptr(200)}
	patches, list := Kustomize([]Change{a, b})
	if len(patches) != 2 || patches[0].Filename == patches[1].Filename {
		t.Fatalf("filenames collide: %+v", patches)
	}
	if strings.Count(list, "path:") != 2 {
		t.Errorf("list = %s", list)
	}
	for _, p := range patches {
		var doc struct {
			Spec struct {
				Template struct {
					Spec struct {
						Containers []map[string]any `yaml:"containers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal([]byte(p.Body), &doc); err != nil {
			t.Fatal(err)
		}
		if _, ok := doc.Spec.Template.Spec.Containers[0]["name"].(string); !ok {
			t.Errorf("%s: container name is not a string:\n%s", p.Filename, p.Body)
		}
	}
}

// resourcesOf decodes one container's resources from a manifest.
func resourcesOf(t *testing.T, manifest, container string) map[string]map[string]string {
	t.Helper()
	var doc struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name      string                       `yaml:"name"`
						Resources map[string]map[string]string `yaml:"resources"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(manifest), &doc); err != nil {
		t.Fatalf("result does not parse: %v\n%s", err, manifest)
	}
	for _, c := range doc.Spec.Template.Spec.Containers {
		if c.Name == container {
			return c.Resources
		}
	}
	t.Fatalf("no container %q", container)
	return nil
}

// A section that exists but lacks one key used to drop the change for that key
// while reporting the container as applied. With requests the last section and
// limits missing, both inserts land after the same line, and applying them one
// at a time put the requests key under limits.
func TestMissingKeysInsideAnExistingSection(t *testing.T) {
	body := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  template:
    spec:
      containers:
        - name: app
          resources:
            requests:
              memory: 1Gi
`
	dir := t.TempDir()
	p := write(t, dir, "api.yaml", body)
	if _, _, err := ApplyTree(dir, []Change{change("app", 300, 600, 512<<20, 640<<20)}, false); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(p)
	got := resourcesOf(t, string(out), "app")
	want := map[string]map[string]string{
		"requests": {"cpu": "300m", "memory": "512Mi"},
		"limits":   {"cpu": "600m", "memory": "640Mi"},
	}
	for sec, kv := range want {
		for k, v := range kv {
			if got[sec][k] != v {
				t.Errorf("%s.%s = %q, want %q\n%s", sec, k, got[sec][k], v, out)
			}
		}
	}
}

// A symlinked manifest may point outside the repository; it is refused, not
// written through, and the refusal names it.
func TestSymlinkedManifestIsNotWrittenThrough(t *testing.T) {
	outside := t.TempDir()
	target := write(t, outside, "api.yaml", deployYAML)
	dir := t.TempDir()
	if err := os.Symlink(target, dir+"/api.yaml"); err != nil {
		t.Skip(err)
	}
	_, missing, err := ApplyTree(dir, []Change{change("app", 300, 600, 512<<20, 640<<20)}, false)
	if err != nil {
		t.Fatal(err)
	}
	if out, _ := os.ReadFile(target); string(out) != deployYAML {
		t.Error("the symlink's target was rewritten")
	}
	if len(missing) != 1 || !strings.Contains(missing[0].Why, "symlink") {
		t.Errorf("missing = %+v", missing)
	}
}
