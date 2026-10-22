package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/procfs"
)

const id64 = "0a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f9"

func TestContainerScope(t *testing.T) {
	tests := []struct {
		path, want string
		ok         bool
	}{
		{"/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podX.slice/cri-containerd-" + id64 + ".scope",
			"/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podX.slice/cri-containerd-" + id64 + ".scope", true},
		// CRI-O runs the processes in a nested cgroup: folded to the scope the kubelet reports
		{"/kubepods.slice/p.slice/crio-" + id64 + ".scope/container", "/kubepods.slice/p.slice/crio-" + id64 + ".scope", true},
		// cgroupfs driver
		{"/kubepods/burstable/podX/" + id64, "/kubepods/burstable/podX/" + id64, true},
		// kubelet and runtime on different cgroup drivers: still a container
		{"/system.slice/containerd.service/kubepods-besteffort-podX.slice:cri-containerd:" + id64,
			"/system.slice/containerd.service/kubepods-besteffort-podX.slice:cri-containerd:" + id64, true},
		{"/kubepods.slice/kubepods-burstable.slice", "/kubepods.slice/kubepods-burstable.slice", false},
		{"/system.slice/sshd.service", "/system.slice/sshd.service", false},
	}
	for _, tt := range tests {
		got, ok := containerScope(tt.path)
		if got != tt.want || ok != tt.ok {
			t.Errorf("containerScope(%q) = %q, %v; want %q, %v", tt.path, got, ok, tt.want, tt.ok)
		}
	}
}

// fakeNode writes a cgroup tree and a /proc tree into temporary directories.
type fakeNode struct {
	t            *testing.T
	cgroup, proc string
}

func newFakeNode(t *testing.T) fakeNode {
	return fakeNode{t: t, cgroup: t.TempDir(), proc: t.TempDir()}
}

func (n fakeNode) write(path, content string) {
	n.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		n.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		n.t.Fatal(err)
	}
}

// container puts the given processes (pid -> thread ids) in a container cgroup.
func (n fakeNode) container(cg string, procs map[int][]int) {
	var pids []string
	for pid, tids := range procs {
		pids = append(pids, itoa(pid))
		for _, tid := range tids {
			// schedstat: run ns, wait ns, timeslices
			n.write(filepath.Join(n.proc, itoa(pid), "task", itoa(tid), "schedstat"), itoa(tid*1000)+" "+itoa(tid)+" 1\n")
		}
	}
	n.write(filepath.Join(n.cgroup, cg, "cgroup.procs"), strings.Join(pids, "\n")+"\n")
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestReaderFindsEveryThreadOfEveryContainer(t *testing.T) {
	n := newFakeNode(t)
	app := "/kubepods.slice/p1.slice/cri-containerd-" + id64 + ".scope"
	crio := "/kubepods.slice/p2.slice/crio-" + strings.Repeat("b", 64) + ".scope"
	outside := "/system.slice/containerd.service/kubepods-p3.slice:cri-containerd:" + strings.Repeat("c", 64)
	n.container(app, map[int][]int{10: {10, 11, 12}}) // one process, three threads
	n.container(crio+"/container", map[int][]int{20: {20}})
	n.container(outside, map[int][]int{30: {30}})
	n.container("/system.slice/sshd.service", map[int][]int{40: {40}}) // not a container

	proc, err := procfs.NewFS(n.proc)
	if err != nil {
		t.Fatal(err)
	}
	w := reader{h: hierarchy{root: n.cgroup, version: "v2"}, proc: proc}.read()

	if len(w.threads) != 5 {
		t.Fatalf("threads = %d, want 5 (3 + 1 + 1, sshd excluded): %v", len(w.threads), w.threads)
	}
	if got := w.threads[11]; got.cgroup != app || got.wait != 11 || got.run != 11000 {
		t.Errorf("thread 11 = %+v", got)
	}
	if got := w.threads[20].cgroup; got != crio {
		t.Errorf("CRI-O thread folded to %q, want %q", got, crio)
	}
	if w.processes[10] != app || w.processes[30] != outside {
		t.Errorf("processes = %v", w.processes)
	}
	if w.containers["kubepods"] != 2 || w.containers["outside"] != 1 {
		t.Errorf("containers = %v, want 2 kubepods + 1 outside", w.containers)
	}
}
