package render

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/2listic/tare/internal/analyze"
	"github.com/2listic/tare/internal/metrics"
	"github.com/2listic/tare/internal/report"
)

func gpuFinding(container string, have ...metrics.Signal) analyze.Finding {
	f := analyze.Finding{
		Namespace: "ml", Workload: "Deployment/" + container, Container: container,
		Verdict: analyze.Waste, Confidence: analyze.Medium, Pods: 1,
		CPURequest: 1000, MemRequest: 4 << 30, RecCPU: 1000, RecMem: 1 << 30, RecMemLimit: 2 << 30,
		GPUs: 1, GPUResource: "nvidia.com/gpu", Have: map[metrics.Signal]bool{},
	}
	for _, s := range have {
		f.Have[s] = true
	}
	return f
}

func gpuReport() *report.Report {
	sleeper := gpuFinding("sleeper", metrics.SigGPUBusy, metrics.SigGPUMem)
	sleeper.GPUIdleDevices, sleeper.GPUMemMax = 1, 8<<20
	sleeper.Reasons = []analyze.Reason{{Code: analyze.GPUIdle}}

	// The lab handover: one sample of 100 among 240 zeros.
	inherited := gpuFinding("inherited", metrics.SigGPUBusy)
	inherited.GPUBusyAvg, inherited.GPUBusyMax = 0.004, 1
	inherited.Reasons = []analyze.Reason{{Code: analyze.GPUUnderused}}

	spinner := gpuFinding("spinner", metrics.SigGPUBusy)
	spinner.GPUBusyAvg, spinner.GPUBusyMax = 0.985, 1
	spinner.Reasons = []analyze.Reason{{Code: analyze.GPUCPUHeld}, {Code: analyze.GPUBusyUnverified}}

	unmeasured := gpuFinding("unmeasured")
	unmeasured.Reasons = []analyze.Reason{{Code: analyze.GPUNotMeasured}}

	shared := gpuFinding("shared", metrics.SigGPUBusy, metrics.SigGPUMem)
	shared.GPUBusyAvg, shared.GPUBusyMax, shared.GPUMemMax, shared.GPUShared = 0.5, 1, 4<<30, true
	shared.Reasons = []analyze.Reason{{Code: analyze.GPUSharedDevice}}

	slice := gpuFinding("slice", metrics.SigGPUBusy)
	slice.GPUResource, slice.GPUBusyMax = "nvidia.com/mig-1g.10gb", 1

	r := sampleReport()
	r.Findings = append(r.Findings, sleeper, inherited, spinner, unmeasured, shared, slice)
	r.Missing = append(r.Missing, analyze.SignalGap{Signal: metrics.SigGPUBusy, Status: analyze.GapSuspect,
		Detail: "no series names a GPU holder"})
	return r
}

func gpuRow(t *testing.T, out, container string) string {
	t.Helper()
	in := false
	for _, line := range strings.Split(out, "\n") {
		switch {
		case line == "# section=gpus":
			in = true
		case line == "" || strings.HasPrefix(line, "# section="):
			in = false
		case in && strings.Contains(line, "Deployment/"+container+" "):
			return line
		}
	}
	t.Fatalf("no gpus row for %s:\n%s", container, out)
	return ""
}

// A measured zero, a tiny number and an unmeasured value are three different
// facts, and the first is the only one that proves a device idle. The
// handover sample that cost the lab sleeper its GPU_IDLE read 0.4% busy, and
// printing it as 0 would show proof the analysis rightly refused.
func TestGPUSectionKeepsZeroApartFromUnmeasured(t *testing.T) {
	out := renderTable(t, gpuReport(), false)
	checkTableShape(t, out, []string{"gpus"})
	for _, tc := range []struct {
		container string
		want      []string
	}{
		{"sleeper", []string{"  1  ", "0/0%", "8Mi", "GPU_IDLE"}},
		{"inherited", []string{"<1/100%"}},
		{"spinner", []string{"98/100%", "GPU_CPU_HELD,GPU_BUSY_UNVERIFIED"}},
		{"unmeasured", []string{"?", "GPU_NOT_MEASURED"}},
	} {
		row := gpuRow(t, out, tc.container)
		for _, w := range tc.want {
			if !strings.Contains(row, w) {
				t.Errorf("%s row lacks %q: %s", tc.container, w, row)
			}
		}
	}
	if row := gpuRow(t, out, "unmeasured"); strings.Contains(row, "  0  ") || strings.Contains(row, "0/0%") {
		t.Errorf("an unmeasured value printed as zero: %s", row)
	}
}

// A shared device's figures belong to every pod on it, and a MIG slice's are
// not yet known to be the slice's. Neither prints a number as this
// container's.
func TestGPUSectionWithholdsOtherPodsNumbers(t *testing.T) {
	out := renderTable(t, gpuReport(), false)
	for _, tc := range []struct{ container, mark string }{{"shared", "shared"}, {"slice", "mig"}} {
		row := gpuRow(t, out, tc.container)
		if strings.Contains(row, "%") || strings.Contains(row, "Gi") {
			t.Errorf("%s printed a figure: %s", tc.container, row)
		}
		if strings.Count(row, tc.mark) < 4 {
			t.Errorf("%s does not say why its figures are withheld: %s", tc.container, row)
		}
	}
}

func TestNoGPUSectionWithoutGPUs(t *testing.T) {
	if out := renderTable(t, sampleReport(), false); strings.Contains(out, "section=gpus") {
		t.Errorf("a report with no GPU container has a gpus section:\n%s", out)
	}
}

// The compact view drops rows, never gaps: an unmeasured GPU and an exporter
// that attributes no device to any pod are both visible without -o wide.
func TestCompactKeepsGPUGaps(t *testing.T) {
	stdout, stderr := renderCompact(t, gpuReport())
	if !strings.Contains(stdout, string(analyze.GPUNotMeasured)) {
		t.Errorf("compact table lost GPU_NOT_MEASURED:\n%s", stdout)
	}
	if !strings.Contains(stderr, "SIGNAL_SUSPECT") || !strings.Contains(stderr, "gpu-busy") {
		t.Errorf("compact warnings lost the GPU attribution control:\n%s", stderr)
	}
}

// -o patch writes cpu and memory and nothing else, whatever the GPU evidence.
func TestPatchNeverWritesGPUs(t *testing.T) {
	f := gpuFinding("trainer", metrics.SigGPUBusy)
	f.GPUs, f.GPUIdleDevices = 2, 1
	f.Reasons = []analyze.Reason{{Code: analyze.GPUDeviceUnused, Text: "holds 2 nvidia.com/gpu, 1 never ran a kernel"}}
	out := capture(t, func(p *printer) { p.printPatches([]analyze.Finding{f}, "", "") })
	var payloads int
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "kubectl") {
			continue
		}
		payloads++
		body := line[strings.Index(line, "-p '")+4 : len(line)-1]
		var patch map[string]any
		if err := json.Unmarshal([]byte(body), &patch); err != nil {
			t.Fatalf("patch payload is not JSON: %v: %s", err, body)
		}
		if strings.Contains(body, "gpu") {
			t.Errorf("patch names a GPU resource: %s", body)
		}
	}
	if payloads != 1 {
		t.Fatalf("want one patch command, got %d:\n%s", payloads, out)
	}
}
