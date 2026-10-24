package report

import (
	"fmt"
	"strings"
	"testing"

	"github.com/2listic/tare/internal/analyze"
	"github.com/2listic/tare/internal/ceilings"
	"github.com/2listic/tare/internal/packing"
)

func redactable() *Report {
	return &Report{
		Source: "apiserver-proxy://monitoring/prometheus-operated:http-web",
		Findings: []analyze.Finding{{
			Namespace: "payments", Workload: "Deployment/ledger-api", Container: "api",
			Image: "registry.example.com/payments/ledger:1.4",
			Nodes: []string{"ip-10-0-3-41"},
			Reasons: []analyze.Reason{
				{Code: analyze.CPUUnderused, Text: "CPU p95 is 7m against a 2000m request"},
				{Code: analyze.NodeContention, Subject: "ip-10-0-3-41",
					Text: "payments/ledger-api ran on ip-10-0-3-41, whose simulator was idle"},
			},
			Ceilings: []ceilings.Ceiling{{Note: "ledger-api is started with -Xmx256m"}},
		}},
		Commitment:    []packing.Commitment{{Node: "ip-10-0-3-41"}},
		DrainBlockers: []string{"payments/ledger-api allows no disruption"},
	}
}

func TestRedactionReachesTheEvidenceAndNotOnlyTheColumns(t *testing.T) {
	r := redactable()
	Redact(r)

	all := strings.Join(append([]string{
		r.Source, r.Findings[0].Namespace, r.Findings[0].Workload, r.Findings[0].Container,
		r.Findings[0].Image, r.Findings[0].Ceilings[0].Note, r.Commitment[0].Node,
	}, append(append(analyze.ReasonTexts(r.Findings[0].Reasons),
		r.Findings[0].Reasons[1].Subject), r.DrainBlockers...)...), "\n")

	for _, secret := range []string{"payments", "ledger-api", "ip-10-0-3-41", "registry.example.com", "monitoring"} {
		if strings.Contains(all, secret) {
			t.Errorf("%q survived redaction:\n%s", secret, all)
		}
	}
	// A redaction that also eats the report is no use: the prose and the
	// numbers have to come through, and so do the controller kinds.
	for _, keep := range []string{"CPU p95 is 7m against a 2000m request", "Deployment/", "-Xmx256m", "names redacted"} {
		if !strings.Contains(all, keep) {
			t.Errorf("%q did not survive redaction:\n%s", keep, all)
		}
	}
}

// A name must not be rewritten where it is only a substring of an ordinary
// word: the header says "simulator", and a namespace called "sim" must not
// turn that into a hash followed by "ulator".
func TestRedactionReplacesWholeNamesOnly(t *testing.T) {
	r := &Report{
		Source:   "simulator (demo data, no cluster was read)",
		Findings: []analyze.Finding{{Namespace: "sim", Workload: "Deployment/w", Container: "c"}},
	}
	Redact(r)
	if !strings.HasPrefix(r.Source, "simulator (") {
		t.Errorf("a substring match mangled the header: %q", r.Source)
	}
}

// The same cluster has to hash the same way every time, or two reports of the
// same workload cannot be compared.
func TestRedactionIsStable(t *testing.T) {
	a, b := redactable(), redactable()
	Redact(a)
	Redact(b)
	if a.Findings[0].Workload != b.Findings[0].Workload {
		t.Errorf("unstable: %q then %q", a.Findings[0].Workload, b.Findings[0].Workload)
	}
	if a.Findings[0].Namespace == "payments" {
		t.Error("nothing was redacted at all")
	}
}

// Custom-resource controller kinds (such as Rollout, Kafka, Prometheus) are API
// type names, not cluster secrets, and hashing them turns "Rollout/api" into
// "27cf1792/44050a29", which no longer reads as kind/name.
func TestRedactionPreservesCustomResourceKinds(t *testing.T) {
	r := &Report{
		Findings: []analyze.Finding{{
			Namespace: "payments", Workload: "Rollout/checkout-api", Container: "api",
			Reasons: []analyze.Reason{{Code: analyze.CPUUnderused, Text: "Rollout/checkout-api in payments"}},
		}},
	}
	Redact(r)
	if !strings.HasPrefix(r.Findings[0].Workload, "Rollout/") {
		t.Errorf("custom controller kind was hashed: %q", r.Findings[0].Workload)
	}
	if strings.Contains(r.Findings[0].Workload, "checkout-api") {
		t.Errorf("workload name was not hashed: %q", r.Findings[0].Workload)
	}
	if !strings.Contains(r.Findings[0].Reasons[0].Text, "Rollout/") {
		t.Errorf("custom controller kind was hashed in reason text: %q", r.Findings[0].Reasons[0].Text)
	}
}

// The strings are what tare printed against the lab cluster under --redact
// when the proxied query failed: the "using" line was hashed and these were not.
func TestScrubberHidesPrometheusEndpointInErrors(t *testing.T) {
	scrub := Scrubber("monitoring", "prometheus-operated")
	for _, raw := range []string{
		"querying apiserver-proxy://monitoring/prometheus-operated:http-web: unknown (get services http:prometheus-operated:http-web)",
		`querying apiserver-proxy://monitoring/prometheus-operated:http-web: Get "https://192.168.1.122:6443/api/v1/namespaces/monitoring/services/http:prometheus-operated:http-web/proxy/api/v1/query?query=sum+by+%28pod%29+%28x%7Bnamespace%3D~%22payments%22%7D%29": context deadline exceeded`,
		`services "http:prometheus-operated:http-web" is forbidden: User "system:serviceaccount:default:x" cannot get resource "services/proxy" in API group "" in the namespace "monitoring"`,
	} {
		got := scrub(raw)
		for _, leak := range []string{"monitoring", "prometheus-operated", "192.168.1.122", "payments"} {
			if strings.Contains(got, leak) {
				t.Errorf("%q survives scrubbing:\n%s", leak, got)
			}
		}
		// The hashes must match the "using" line, or the reader sees two endpoints.
		if !strings.Contains(got, HashName("monitoring")) && !strings.Contains(got, HashName("192.168.1.122")) {
			t.Errorf("scrubbed text carries neither hash of the using line:\n%s", got)
		}
	}
	if got := scrub("context deadline exceeded"); got != "context deadline exceeded" {
		t.Errorf("text with no endpoint was changed: %q", got)
	}
}

// Checking a list of fields passes for any field nobody thought to list, which
// is how PDB names, HPA names and the pin reasons all reached a redacted lab
// report. This reads every field of the report instead, so a field added
// later is covered without anyone remembering to.
func TestRedactionLeavesNoNameAnywhereInTheReport(t *testing.T) {
	r := redactable()
	r.Findings[0].Constraints.HPAName = "ledger-hpa"
	r.Findings[0].Constraints.VPAName = "ledger-vpa"
	r.DrainBlockers = []string{"PodDisruptionBudget payments/ledger-pdb requires all 2 of its pods healthy"}
	r.Nodes.PinReasons = map[string]int{
		"PodDisruptionBudget payments/ledger-pdb allows no eviction, so its node cannot be drained": 2,
		"has a nodeSelector": 3,
	}
	Redact(r)
	out := fmt.Sprintf("%+v", *r)
	for _, secret := range []string{"payments", "ledger-api", "ledger-pdb", "ledger-hpa", "ledger-vpa", "ip-10-0-3-41", "monitoring", "prometheus-operated"} {
		if strings.Contains(out, secret) {
			t.Errorf("%q survived redaction", secret)
		}
	}
	if r.Nodes.PinReasons["has a nodeSelector"] != 3 {
		t.Errorf("a pin reason with no name in it was lost: %v", r.Nodes.PinReasons)
	}
	// The header and the "using" line on stderr hash the endpoint
	// independently; if they disagree the reader sees two Prometheuses.
	if want := HashName("monitoring") + "/" + HashName("prometheus-operated"); !strings.Contains(r.Source, want) {
		t.Errorf("source %q does not carry %q, the hash the using line prints", r.Source, want)
	}
}
