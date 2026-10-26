package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/mfranzon/tare/internal/discover"
	"github.com/mfranzon/tare/internal/metrics"
)

// fakeProm answers the counting queries preflight makes. absent names the
// metric families this cluster does not export; zeroPressure makes the
// pressure family exist with nothing ever non-zero, which is the kubelet bug
// the control exists for.
func fakeProm(t *testing.T, absent []string, zeroPressure bool) *httptest.Server {
	t.Helper()
	missing := map[string]bool{}
	for _, m := range absent {
		missing[m] = true
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		if strings.Contains(q, "count by (namespace,pod,container)") {
			fmt.Fprint(w, `{"status":"success","data":{"result":[{"metric":`+
				`{"namespace":"prod","pod":"p1","container":"app"},"value":[0,"1"]}]}}`)
			return
		}
		value := "12"
		for m := range missing {
			if strings.Contains(q, m) {
				value = ""
			}
		}
		// The non-zero-pressure probe is the one query wrapped in "> 0".
		if strings.Contains(q, "container_pressure") && strings.Contains(q, "> 0") && zeroPressure {
			value = ""
		}
		// A complete cluster has no containers outside kubepods.
		if strings.Contains(q, `location="outside"`) {
			value = ""
		}
		if value == "" {
			fmt.Fprint(w, `{"status":"success","data":{"result":[]}}`)
			return
		}
		fmt.Fprintf(w, `{"status":"success","data":{"result":[{"metric":{},"value":[0,"%s"]}]}}`, value)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func checkClient(allow bool) *fake.Clientset {
	cs := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "prod"},
		Spec:       corev1.PodSpec{NodeName: "n1", Containers: []corev1.Container{{Name: "app"}}},
	})
	cs.PrependReactor("create", "selfsubjectaccessreviews",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, &authv1.SelfSubjectAccessReview{
				Status: authv1.SubjectAccessReviewStatus{Allowed: allow},
			}, nil
		})
	return cs
}

func runCheckString(t *testing.T, cs *fake.Clientset, promURL string) (string, error) {
	t.Helper()
	var b strings.Builder
	err := runCheck(context.Background(), &b, discover.NewWithClientset(cs), nil, promURL, 14*24*time.Hour, false)
	return b.String(), err
}

func TestCheckPassesOnACompleteCluster(t *testing.T) {
	out, err := runCheckString(t, checkClient(true), fakeProm(t, nil, false).URL)
	if err != nil {
		t.Fatalf("check failed on a cluster with everything: %v\n%s", err, out)
	}
	for _, want := range []string{"container_cpu_usage_seconds_total", "ready", "list pods"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "✗") {
		t.Errorf("a complete cluster reported a failure:\n%s", out)
	}
}

// A missing required metric family has to stop the run. Reporting on a backend
// that carries no container CPU at all would produce a page of recommendations
// sized from nothing.
func TestCheckFailsWhenARequiredSeriesIsAbsent(t *testing.T) {
	out, err := runCheckString(t, checkClient(true),
		fakeProm(t, []string{"container_cpu_usage_seconds_total"}, false).URL)
	if err == nil {
		t.Fatalf("check passed without container CPU metrics:\n%s", out)
	}
	if !strings.Contains(err.Error(), "container_cpu_usage_seconds_total") {
		t.Errorf("error does not name the missing metric: %v", err)
	}
}

// An optional family costs a named part of the report and nothing more.
func TestCheckDegradesOnAnOptionalSeries(t *testing.T) {
	out, err := runCheckString(t, checkClient(true),
		fakeProm(t, []string{"node_cpu_seconds_total"}, false).URL)
	if err != nil {
		t.Fatalf("a missing node-exporter must not be fatal: %v", err)
	}
	if !strings.Contains(out, "node_cpu_seconds_total") || !strings.Contains(out, "ready, with gaps") {
		t.Errorf("degradation was not reported:\n%s", out)
	}
}

// The dead pressure instrument: the series are there, every value is zero, and
// zero is the reassuring answer. check must say so before a report reads it as
// a calm cluster.
func TestCheckFlagsPressureThatIsPresentAndAlwaysZero(t *testing.T) {
	out, err := runCheckString(t, checkClient(true), fakeProm(t, nil, true).URL)
	if err != nil {
		t.Fatalf("unexpected failure: %v", err)
	}
	if !strings.Contains(out, "136333") {
		t.Errorf("all-zero pressure was not flagged:\n%s", out)
	}
}

// A namespace-scoped token has to be told which permission it is missing and
// what that costs, not handed a Kubernetes error later on.
func TestCheckNamesTheMissingPermissions(t *testing.T) {
	out, err := runCheckString(t, checkClient(false), fakeProm(t, nil, false).URL)
	if err == nil {
		t.Fatalf("check passed with no permissions at all:\n%s", out)
	}
	if !strings.Contains(out, "list pods") || !strings.Contains(out, "list nodes") {
		t.Errorf("output does not enumerate the denied reads:\n%s", out)
	}
	if !strings.Contains(out, "node-count delta") {
		t.Errorf("output does not say what a denied read costs:\n%s", out)
	}
}

// tare-agent is a cluster admin's install, so check says when it is needed,
// what is lost without it, and what is wrong with it once it is there.
func TestCheckAgent(t *testing.T) {
	cases := []struct {
		name  string
		st    metrics.AgentStatus
		psiOK bool
		ok    bool
		want  string
	}{
		{"psi, no agent", metrics.AgentStatus{}, true, true, "not needed"},
		{"no psi, no agent", metrics.AgentStatus{}, false, false, "deploy/agent/agent.yaml"},
		{"healthy", metrics.AgentStatus{Nodes: 3, Matched: 40}, false, true, "passing on every node"},
		{"broken join", metrics.AgentStatus{Nodes: 3}, false, false, "`id` label"},
		{"driver mismatch", metrics.AgentStatus{Nodes: 3, Matched: 40, Outside: 2}, false, false, "cgroup drivers"},
		{"failing node", metrics.AgentStatus{Nodes: 3, Matched: 40, Failing: []metrics.AgentNode{
			{Instance: "10.0.0.7:9105", Problems: []string{"exit records dropped by the kernel"}}}}, false, false, "10.0.0.7:9105"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var b strings.Builder
			if got := checkAgent(&b, c.st, c.psiOK); got != c.ok {
				t.Errorf("ok = %v, want %v:\n%s", got, c.ok, b.String())
			}
			if !strings.Contains(strings.Join(strings.Fields(b.String()), " "), c.want) {
				t.Errorf("output lacks %q:\n%s", c.want, b.String())
			}
		})
	}
}
