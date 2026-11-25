package metrics

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// dcgmSeries is one device series as Prometheus returns it after the
// `by (UUID, exported_*, namespace, pod, container)` aggregation. The label
// shapes are the ones measured on the lab cluster on 2026-10-29: under the
// default ServiceMonitor, namespace/pod/container are the exporter's own, and
// the pod holding the device is only in exported_*.
type dcgmSeries struct {
	labels                 map[string]string
	util, utilAvg, sm, mem float64
	first, last            float64
}

const (
	labUUID     = "GPU-82b2f36e-d38a-9666-22bc-42e4f5db6972"
	labExporter = "dcgm-dcgm-exporter-8b59l"
	t0          = 1761700000.0
)

// heldBy is a series under honorLabels: false, the chart default.
func heldBy(ns, pod, container, uuid string) map[string]string {
	return map[string]string{
		"UUID": uuid, "namespace": "nvidia", "pod": labExporter, "container": "exporter",
		"exported_namespace": ns, "exported_pod": pod, "exported_container": container,
	}
}

// unheld is a device no pod holds: only the exporter's own labels.
func unheld(uuid string) map[string]string {
	return map[string]string{"UUID": uuid, "namespace": "nvidia", "pod": labExporter, "container": "exporter"}
}

func dcgmProm(t *testing.T, noSM bool, series ...dcgmSeries) *fakeProm {
	t.Helper()
	return newFakeProm(func(expr string) (string, int) {
		pick := func(d dcgmSeries) (float64, bool) {
			switch {
			case strings.Contains(expr, "DCGM_FI_PROF_SM_ACTIVE"):
				return d.sm, !noSM
			case strings.Contains(expr, "DCGM_FI_DEV_FB_USED"):
				return d.mem, true
			case strings.Contains(expr, "min_over_time(timestamp("):
				return d.first, true
			case strings.Contains(expr, "max_over_time(timestamp("):
				return d.last, true
			case strings.Contains(expr, "avg_over_time(DCGM_FI_DEV_GPU_UTIL"):
				return d.utilAvg, true
			case strings.Contains(expr, "DCGM_FI_DEV_GPU_UTIL"):
				return d.util, true
			}
			return 0, false
		}
		var result []any
		for _, d := range series {
			if v, ok := pick(d); ok {
				result = append(result, map[string]any{"metric": d.labels, "value": []any{t0, formatFloat(v)}})
			}
		}
		b, _ := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}})
		return string(b), 200
	})
}

func observeGPUs(t *testing.T, f *fakeProm, holders map[Key]bool, ns ...string) (map[Key]GPUObservation, []SignalHealth) {
	t.Helper()
	defer f.Close()
	p := NewProm(f.URL)
	out, err := p.ObserveGPUs(holders, ns, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return out, p.Health()
}

func gpuSuspect(h []SignalHealth) string {
	for _, s := range h {
		if s.Signal == SigGPUBusy {
			return s.Suspect
		}
	}
	return ""
}

var (
	sleeper = Key{"gpu-lab", "gpu-sleeper-6d8c899779-bcgm9", "sleeper"}
	spinner = Key{"gpu-lab", "gpu-spinner-6469d7f5d7-s4chs", "spinner"}
)

// The defect this file exists for: a namespace/pod/container join reads the
// exporter's labels and credits the GPU to the exporter.
func TestGPUIsCreditedToItsHolderNotTheExporter(t *testing.T) {
	obs, h := observeGPUs(t, dcgmProm(t, true,
		dcgmSeries{labels: heldBy(spinner.Namespace, spinner.Pod, spinner.Container, labUUID),
			util: 100, utilAvg: 100, mem: 121, first: t0 - 3600, last: t0},
	), map[Key]bool{spinner: true})

	if _, ok := obs[Key{"nvidia", labExporter, "exporter"}]; ok {
		t.Fatal("the GPU was credited to dcgm-exporter itself")
	}
	o, ok := obs[spinner]
	if !ok {
		t.Fatalf("spinner not attributed: %v", obs)
	}
	if o.BusyMax != 1 || !o.HasBusy {
		t.Errorf("busy %v (has %v), want 1.0: GPU_UTIL is 0-100 and must arrive as a fraction", o.BusyMax, o.HasBusy)
	}
	if o.MemMax != 121<<20 {
		t.Errorf("memory %v, want 121MiB in bytes", o.MemMax)
	}
	if o.HasSM {
		t.Error("SM activity reported present on a card that exports no profiling metrics")
	}
	if !slices.Equal(o.Devices, []string{labUUID}) || len(o.IdleDevices) != 0 {
		t.Errorf("devices %v idle %v", o.Devices, o.IdleDevices)
	}
	if gpuSuspect(h) != "" {
		t.Errorf("a working exporter was marked suspect: %s", gpuSuspect(h))
	}
}

// Under honorLabels: true the owner is in the plain labels. They are believed
// for a known GPU holder, and only for one.
func TestHonorLabelsTrustsOnlyKnownHolders(t *testing.T) {
	plain := func(k Key) map[string]string {
		return map[string]string{"UUID": labUUID, "namespace": k.Namespace, "pod": k.Pod, "container": k.Container}
	}
	stranger := Key{"web", "nginx-0", "nginx"}
	obs, _ := observeGPUs(t, dcgmProm(t, true,
		dcgmSeries{labels: plain(sleeper), util: 0, first: t0 - 3600, last: t0},
		dcgmSeries{labels: plain(stranger), util: 50, first: t0 - 3600, last: t0},
	), map[Key]bool{sleeper: true})

	if _, ok := obs[sleeper]; !ok {
		t.Error("a known holder named by plain labels was not attributed")
	}
	if _, ok := obs[stranger]; ok {
		t.Error("a container that holds no GPU was credited with one from plain labels")
	}
}

func TestIdleDeviceIsNamed(t *testing.T) {
	obs, _ := observeGPUs(t, dcgmProm(t, true,
		dcgmSeries{labels: heldBy(sleeper.Namespace, sleeper.Pod, sleeper.Container, labUUID), first: t0 - 3600, last: t0},
		dcgmSeries{labels: heldBy(sleeper.Namespace, sleeper.Pod, sleeper.Container, "GPU-other"), util: 40, utilAvg: 20, first: t0 - 3600, last: t0},
	), map[Key]bool{sleeper: true})

	o := obs[sleeper]
	if !slices.Equal(o.IdleDevices, []string{labUUID}) {
		t.Errorf("idle devices %v, want only the one whose busy-time never left zero", o.IdleDevices)
	}
	if o.BusyMax != 0.4 {
		t.Errorf("busy max %v, want the worst device's 0.4", o.BusyMax)
	}
}

// The negative control: GPU pods exist, the exporter answers, and no series
// names any of them. That is an exporter that cannot see pods, and reading it
// as "no use" would make every GPU in the cluster look idle.
func TestUnattributedExporterIsSuspectNotIdle(t *testing.T) {
	obs, h := observeGPUs(t, dcgmProm(t, true,
		dcgmSeries{labels: unheld(labUUID), util: 0, first: t0 - 3600, last: t0},
	), map[Key]bool{sleeper: true})

	if len(obs) != 0 {
		t.Errorf("observations from an exporter that attributes nothing: %v", obs)
	}
	if gpuSuspect(h) == "" {
		t.Error("an exporter attributing no device to any GPU pod was not marked suspect")
	}
}

// Must not fire: an unheld device beside a held one is the ordinary state of a
// node with spare GPUs.
func TestSpareDeviceIsNotSuspect(t *testing.T) {
	_, h := observeGPUs(t, dcgmProm(t, true,
		dcgmSeries{labels: unheld("GPU-spare"), first: t0 - 3600, last: t0},
		dcgmSeries{labels: heldBy(spinner.Namespace, spinner.Pod, spinner.Container, labUUID), util: 100, first: t0 - 3600, last: t0},
	), map[Key]bool{spinner: true})
	if s := gpuSuspect(h); s != "" {
		t.Errorf("spare device condemned the exporter: %s", s)
	}
}

// Must not fire: a namespace filter that excludes every GPU pod is a scoped
// report, not a broken exporter.
func TestNamespaceFilterIsNotSuspect(t *testing.T) {
	obs, h := observeGPUs(t, dcgmProm(t, true,
		dcgmSeries{labels: heldBy(spinner.Namespace, spinner.Pod, spinner.Container, labUUID), util: 100, first: t0 - 3600, last: t0},
	), map[Key]bool{spinner: true}, "web")
	if len(obs) != 0 {
		t.Errorf("a gpu-lab pod leaked through -n web: %v", obs)
	}
	if s := gpuSuspect(h); s != "" {
		t.Errorf("a namespace filter condemned the exporter: %s", s)
	}
}

// Two pods holding one device at once is time-slicing or MPS; one handing it
// to the next is a rollout, which is what the lab's sleeper/spinner swap did.
func TestSharingIsOverlapNotSuccession(t *testing.T) {
	cases := []struct {
		name   string
		second [2]float64
		shared bool
	}{
		{"concurrent", [2]float64{t0 - 3000, t0}, true},
		{"handed over", [2]float64{t0 - 1800 - 30, t0}, false}, // one scrape of overlap
		{"later", [2]float64{t0 - 1000, t0}, false},
	}
	for _, c := range cases {
		obs, _ := observeGPUs(t, dcgmProm(t, true,
			dcgmSeries{labels: heldBy(sleeper.Namespace, sleeper.Pod, sleeper.Container, labUUID), first: t0 - 3600, last: t0 - 1800},
			dcgmSeries{labels: heldBy(spinner.Namespace, spinner.Pod, spinner.Container, labUUID), util: 100, first: c.second[0], last: c.second[1]},
		), map[Key]bool{sleeper: true, spinner: true})
		got := len(obs[spinner].SharedWith) > 0
		if got != c.shared {
			t.Errorf("%s: shared=%v (%v), want %v", c.name, got, obs[spinner].SharedWith, c.shared)
		}
	}
}

func TestNoProfilingIsAbsentNotZero(t *testing.T) {
	obs, h := observeGPUs(t, dcgmProm(t, true,
		dcgmSeries{labels: heldBy(spinner.Namespace, spinner.Pod, spinner.Container, labUUID), util: 100, first: t0 - 3600, last: t0},
	), map[Key]bool{spinner: true})
	if obs[spinner].HasSM {
		t.Error("no SM series, yet SM activity is marked present: 0 would read as an unused GPU")
	}
	for _, s := range h {
		if s.Signal == SigGPUSMActive && s.OK() {
			t.Error("SM activity health reports OK with no series")
		}
	}
}

func TestDatacenterCardReportsSMActivity(t *testing.T) {
	obs, _ := observeGPUs(t, dcgmProm(t, false,
		dcgmSeries{labels: heldBy(spinner.Namespace, spinner.Pod, spinner.Container, labUUID), util: 100, sm: 0.03, first: t0 - 3600, last: t0},
	), map[Key]bool{spinner: true})
	o := obs[spinner]
	if !o.HasSM || o.SMActiveP95 != 0.03 {
		t.Errorf("SM p95 %v (has %v), want 0.03", o.SMActiveP95, o.HasSM)
	}
}
