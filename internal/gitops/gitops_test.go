package gitops

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/2listic/tare/internal/analyze"
)

const deployYAML = `# Owned by the checkout team. Ping #checkout before changing.
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  namespace: prod
  labels:
    app: api          # used by the service selector
spec:
  replicas: 3
  template:
    spec:
      containers:
        - name: app
          image: api:1.4
          resources:
            requests:
              cpu: 2      # bumped during the incident, never reverted
              memory: 4Gi
            limits:
              cpu: 4
              memory: 8Gi
        - name: sidecar
          image: proxy:0.9
`

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func change(container string, cpuReq, cpuLim, memReq, memLim int64) Change {
	return Change{
		Namespace: "prod", Kind: "Deployment", Name: "api", Container: container,
		CPURequest: ptr(cpuReq), CPULimit: ptr(cpuLim),
		MemRequest: ptr(memReq), MemLimit: ptr(memLim),
	}
}

// The property that decides whether the diff is merged: a colleague's comments
// and key order survive. A round trip through Go structs loses both, and a pull
// request that silently deletes someone's incident note will be rejected on
// sight however correct its numbers are.
func TestEditPreservesCommentsAndOrder(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "api.yaml", deployYAML)

	if _, _, err := ApplyTree(dir, []Change{change("app", 300, 600, 512<<20, 640<<20)}, false); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	out := string(got)

	for _, comment := range []string{
		"Owned by the checkout team",
		"used by the service selector",
		"bumped during the incident, never reverted",
	} {
		if !strings.Contains(out, comment) {
			t.Errorf("comment lost: %q\n%s", comment, out)
		}
	}
	if i, j := strings.Index(out, "replicas:"), strings.Index(out, "template:"); i < 0 || j < 0 || i > j {
		t.Errorf("key order changed:\n%s", out)
	}
	if !strings.Contains(out, "cpu: 300m") || !strings.Contains(out, "memory: 512Mi") {
		t.Errorf("new values not written:\n%s", out)
	}
	if !strings.Contains(out, "cpu: 600m") || !strings.Contains(out, "memory: 640Mi") {
		t.Errorf("limits not written:\n%s", out)
	}
	// The second container was not in the plan and must be untouched.
	if strings.Contains(out, "proxy:0.9\n          resources:") {
		t.Errorf("a container with no change must not gain a resources block:\n%s", out)
	}
}

// A container with no resources block at all is the most valuable case to
// change and the one most likely to be missing the keys entirely.
func TestEditCreatesMissingResourceBlocks(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "api.yaml", deployYAML)
	if _, _, err := ApplyTree(dir, []Change{change("sidecar", 50, 100, 64<<20, 80<<20)}, false); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(p)
	if !strings.Contains(string(out), "cpu: 50m") {
		t.Errorf("a container without a resources block must gain one:\n%s", out)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "api.yaml", deployYAML)
	edits, _, err := ApplyTree(dir, []Change{change("app", 300, 600, 512<<20, 640<<20)}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(edits) != 1 || edits[0].After == edits[0].Before {
		t.Fatal("a dry run must still compute the change")
	}
	after, _ := os.ReadFile(p)
	if string(after) != deployYAML {
		t.Error("a dry run must not touch the working tree")
	}
}

// Helm templates are not YAML until rendered. Editing one produces a corrupt
// file, which is worse than not editing it.
func TestHelmTemplatesAreLeftAlone(t *testing.T) {
	dir := t.TempDir()
	tpl := strings.Replace(deployYAML, "  name: api", "  name: {{ .Release.Name }}-api", 1)
	p := write(t, dir, "templates/api.yaml", tpl)
	_, missing, err := ApplyTree(dir, []Change{change("app", 300, 600, 512<<20, 640<<20)}, false)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(p)
	if string(after) != tpl {
		t.Error("a Helm template must not be rewritten")
	}
	if len(missing) != 1 || !strings.Contains(missing[0].Why, "Helm") {
		t.Errorf("an unmatched workload must be reported, naming Helm: %+v", missing)
	}
}

// Silence would let a reviewer believe the diff covers the whole report.
func TestWorkloadsNotInTheRepoAreReported(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "other.yaml", deployYAML)
	c := change("app", 300, 600, 512<<20, 640<<20)
	c.Name = "not-in-this-repo"
	_, missing, err := ApplyTree(dir, []Change{c}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 {
		t.Fatalf("an unmatched workload must be reported, got %d", len(missing))
	}
}

// A no-op edit must not produce a diff: a pull request whose files are
// unchanged wastes the reviewer's attention and erodes trust in the next one.
func TestUnchangedValuesProduceNoEdit(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "api.yaml", deployYAML)
	// The manifest already says 2 CPU / 4Gi request, 4 CPU / 8Gi limit.
	edits, _, err := ApplyTree(dir, []Change{change("app", 2000, 4000, 4<<30, 8<<30)}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(edits) != 0 {
		t.Errorf("writing the values already present must produce no diff, got %d edits:\n%s",
			len(edits), edits[0].After)
	}
}

func TestNamespaceMismatchIsNotEdited(t *testing.T) {
	dir := t.TempDir()
	c := change("app", 300, 600, 512<<20, 640<<20)
	c.Namespace = "staging" // the file says prod
	if _, _, err := ApplyTree(dir, []Change{c}, true); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "api.yaml", deployYAML)
	edits, _, _ := ApplyTree(dir, []Change{c}, true)
	if len(edits) != 0 {
		t.Error("a manifest in a different namespace must not be edited")
	}
}

// --- planning --------------------------------------------------------------

func TestPlanDropsWhatTareWillNotDefend(t *testing.T) {
	fs := []analyze.Finding{
		{Namespace: "prod", Workload: "Deployment/ok", Container: "c", Verdict: analyze.Waste, RecCPU: 100, RecMem: 1 << 20},
		{Namespace: "prod", Workload: "Deployment/leak", Container: "c", Verdict: analyze.Leaking, RecCPU: 100, RecMem: 1 << 20},
		{Namespace: "prod", Workload: "Job/batch", Container: "c", Verdict: analyze.Waste, RecCPU: 100, RecMem: 1 << 20},
	}
	changes, skipped := Plan(fs)
	if len(changes) != 1 || changes[0].Name != "ok" {
		t.Fatalf("only the defendable finding may be changed: %+v", changes)
	}
	joined := ""
	for _, s := range skipped {
		joined += s.Why + " | "
	}
	if !strings.Contains(joined, "LEAKING") || !strings.Contains(joined, "immutable") {
		t.Errorf("both refusals must be explained: %s", joined)
	}
}

// A Guaranteed pod whose requests and limits would diverge must not be written
// into a repo: the diff looks harmless and silently demotes its QoS class.
func TestGuaranteedPodIsNotSilentlyDemoted(t *testing.T) {
	_, skipped := Plan([]analyze.Finding{{
		Namespace: "prod", Workload: "Deployment/g", Container: "c", Verdict: analyze.Waste,
		QoS: "Guaranteed", RecCPU: 100, RecCPULimit: 400, RecMem: 1 << 20, RecMemLimit: 2 << 20,
	}})
	if len(skipped) != 1 || !strings.Contains(skipped[0].Why, "Burstable") {
		t.Errorf("a QoS demotion must be refused and named: %+v", skipped)
	}
}

func TestPullRequestBodyCarriesTheEvidence(t *testing.T) {
	c := change("app", 300, 600, 512<<20, 640<<20)
	c.Reasons = []string{"CPU p95 is 120m against a 2000m request"}
	body := PullRequestBody([]Change{c}, []Skipped{{"prod/Deployment/x", "c", "verdict LEAKING"}}, "prometheus", "14d")
	for _, want := range []string{"requests.cpu", "`300m`", "CPU p95 is 120m", "Deliberately not changed", "verdict LEAKING"} {
		if !strings.Contains(body, want) {
			t.Errorf("PR body missing %q:\n%s", want, body)
		}
	}
}

func TestQuantityRendering(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{{2000, "2"}, {300, "300m"}, {10, "10m"}, {0, "0"}}
	for _, tc := range cases {
		if got := CPUQuantity(tc.in); got != tc.want {
			t.Errorf("CPUQuantity(%d) = %q want %q", tc.in, got, tc.want)
		}
	}
	memCases := []struct {
		in   int64
		want string
	}{{1 << 30, "1Gi"}, {512 << 20, "512Mi"}, {1536 << 20, "1536Mi"}, {0, "0"}}
	for _, tc := range memCases {
		if got := MemQuantity(tc.in); got != tc.want {
			t.Errorf("MemQuantity(%d) = %q want %q", tc.in, got, tc.want)
		}
	}
}

// A Kustomize patch addresses the object by kind and name, which is exactly
// what a finding carries. That is why this path needs no mapping from a
// rendered object back to the source that produced it.
func TestKustomizePatchAddressesTheObject(t *testing.T) {
	c := change("app", 300, 600, 512<<20, 640<<20)
	patches, list := Kustomize([]Change{c})
	if len(patches) != 1 {
		t.Fatalf("expected one patch, got %d", len(patches))
	}
	body := patches[0].Body
	for _, want := range []string{"kind: Deployment", "name: api", "namespace: prod", `- name: "app"`, `cpu: "300m"`, `memory: "640Mi"`} {
		if !strings.Contains(body, want) {
			t.Errorf("patch missing %q:\n%s", want, body)
		}
	}
	if !strings.Contains(list, "patches:") || !strings.Contains(list, patches[0].Filename) {
		t.Errorf("the kustomization entry must name the file: %s", list)
	}
}

func TestKustomizeGroupsContainersOfOneWorkload(t *testing.T) {
	patches, _ := Kustomize([]Change{change("app", 300, 600, 512<<20, 640<<20), change("sidecar", 50, 100, 64<<20, 80<<20)})
	if len(patches) != 1 {
		t.Fatalf("two containers of one Deployment are one patch, got %d", len(patches))
	}
	if !strings.Contains(patches[0].Body, `- name: "app"`) || !strings.Contains(patches[0].Body, `- name: "sidecar"`) {
		t.Errorf("both containers must appear:\n%s", patches[0].Body)
	}
}

// The property that makes the output reviewable at all, and the one the first
// implementation failed: every line the change does not touch comes out
// byte-identical. Re-encoding through yaml.Node preserved comments and order
// and still reindented every sequence, turning a two-line edit into a 117-line
// diff on a real manifest. A reviewer rejects that without reading it.
func TestOnlyChangedLinesDiffer(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "api.yaml", deployYAML)
	if _, _, err := ApplyTree(dir, []Change{change("app", 300, 600, 512<<20, 640<<20)}, false); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)

	before := strings.Split(deployYAML, "\n")
	after := strings.Split(string(got), "\n")
	if len(before) != len(after) {
		t.Fatalf("line count changed %d -> %d; no key was added here:\n%s", len(before), len(after), got)
	}
	var differing []string
	for i := range before {
		if before[i] != after[i] {
			differing = append(differing, fmt.Sprintf("%d: %q -> %q", i+1, before[i], after[i]))
		}
	}
	// Exactly four values were asked for, so exactly four lines may move.
	if len(differing) != 4 {
		t.Errorf("expected 4 changed lines, got %d:\n%s", len(differing), strings.Join(differing, "\n"))
	}
}

// A trailing comment on the very line being edited must survive: that is where
// the note explaining the current value usually lives, and deleting it removes
// the reviewer's context for the change.
func TestTrailingCommentOnAnEditedLineSurvives(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "api.yaml", deployYAML)
	if _, _, err := ApplyTree(dir, []Change{change("app", 300, 600, 512<<20, 640<<20)}, false); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if !strings.Contains(string(got), "cpu: 300m      # bumped during the incident, never reverted") {
		t.Errorf("the comment beside the edited value must stay on its line:\n%s", got)
	}
}

const flowStyleYAML = `apiVersion: apps/v1
kind: Deployment
metadata: {name: api, namespace: prod}
spec:
  template:
    spec:
      containers:
      - name: c
        image: nginx:1.27-alpine
        resources:
          requests: {cpu: 10m, memory: 16Mi}
          limits: {cpu: "14", memory: 6Gi}
`

// The bug a real repository found and the unit tests had not: on a flow-style
// mapping the value token runs to the closing brace, not to the end of the
// line. Swallowing the brace writes YAML that no longer parses, which is the
// worst thing a tool that edits your repository can do.
func TestFlowStyleMappingKeepsItsBraces(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "api.yaml", flowStyleYAML)
	if _, _, err := ApplyTree(dir, []Change{change("c", 300, 600, 512<<20, 640<<20)}, false); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	out := string(got)
	if !strings.Contains(out, "{cpu: 300m, memory: 512Mi}") {
		t.Errorf("flow mapping mangled:\n%s", out)
	}
	if !strings.Contains(out, `{cpu: 600m, memory: 640Mi}`) {
		t.Errorf("quoted flow value mangled:\n%s", out)
	}
	if err := parses(out); err != nil {
		t.Fatalf("the edit produced unparseable YAML: %v\n%s", err, out)
	}
}

func TestValueToken(t *testing.T) {
	cases := []struct{ in, want string }{
		{`1Gi}`, "1Gi"},
		{`10m, memory: 16Mi}`, "10m"},
		{`"14", memory: 6Gi}`, `"14"`},
		{`2      # bumped during the incident`, "2"},
		{`512Mi`, "512Mi"},
		{`'2'`, `'2'`},
		{`4Gi]`, "4Gi"},
	}
	for _, tc := range cases {
		if got := valueToken(tc.in); got != tc.want {
			t.Errorf("valueToken(%q) = %q want %q", tc.in, got, tc.want)
		}
	}
}

// The guard that turns a future tokenising mistake into a skipped file rather
// than a corrupted one.
func TestUnparseableResultIsRefused(t *testing.T) {
	if err := parses("a: [1, 2\nb: {"); err == nil {
		t.Error("broken YAML must not pass the parse check")
	}
	if err := parses("a: 1\n---\nb: 2\n"); err != nil {
		t.Errorf("a valid multi-document stream must pass: %v", err)
	}
}
