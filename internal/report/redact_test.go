package report

import (
	"strings"
	"testing"

	"github.com/mfranzon/tare/internal/analyze"
	"github.com/mfranzon/tare/internal/ceilings"
	"github.com/mfranzon/tare/internal/packing"
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
