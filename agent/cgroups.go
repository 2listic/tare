package main

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/containerd/cgroups/v3"
	"github.com/prometheus/procfs"
)

// containerID matches a cgroup path that has a component carrying a 64-hex
// container id (cri-containerd-<id>.scope, crio-<id>.scope, docker-<id>.scope,
// cgroupfs <id>, or the systemd triple ...:cri-containerd:<id> of a driver
// mismatch). Group 1 is the path up to and including that component.
var containerID = regexp.MustCompile(`^(.*?/[^/]*[0-9a-f]{64}[^/]*)(/.*)?$`)

// containerScope folds a cgroup path into the container it belongs to: the path
// is cut after the component carrying the container id. CRI-O runs a container's
// processes in crio-<id>.scope/container while the kubelet reports
// crio-<id>.scope. ok is false for paths that belong to no container.
func containerScope(path string) (scope string, ok bool) {
	m := containerID.FindStringSubmatch(path)
	if m == nil {
		return path, false
	}
	return m[1], true
}

// location tells where a container cgroup lives: under the kubelet's kubepods
// hierarchy, or outside it. Outside happens when the kubelet uses the systemd
// cgroup driver and the runtime cgroupfs: the runtime then creates the container
// under its own service, /system.slice/containerd.service/....
func location(scope string) string {
	// The top-level directory decides: a driver mismatch puts "kubepods" inside
	// /system.slice/containerd.service/kubepods-...:cri-containerd:<id>.
	if strings.HasPrefix(scope, "/kubepods") {
		return "kubepods"
	}
	return "outside"
}

// hierarchy is the cgroup tree the agent walks: the unified tree on cgroup v2,
// the cpu,cpuacct tree on v1 (and hybrid, where the cpu controller is on v1).
type hierarchy struct {
	root    string // directory on disk, e.g. /sys/fs/cgroup
	version string // "v1" or "v2"
}

func detectHierarchy(mount string) hierarchy {
	if cgroups.Mode() == cgroups.Unified {
		return hierarchy{root: mount, version: "v2"}
	}
	for _, name := range []string{"cpu,cpuacct", "cpuacct,cpu", "cpu"} {
		if st, err := os.Stat(filepath.Join(mount, name)); err == nil && st.IsDir() {
			return hierarchy{root: filepath.Join(mount, name), version: "v1"}
		}
	}
	return hierarchy{root: mount, version: "v1"}
}

// thread is one live task as read from its schedstat file.
type thread struct {
	cgroup string // container scope
	wait   uint64 // ns spent runnable but waiting for a CPU (sched_info.run_delay)
	run    uint64 // ns spent running
}

// walk is one pass over every container: the live threads, and which container
// each process (tgid) belongs to.
type walk struct {
	threads    map[int]thread // tid -> thread
	processes  map[int]string // tgid -> container scope
	containers map[string]int // location -> number of containers
}

// reader reads cgroups and /proc. Paths are parameters so tests can point it at
// fake trees.
type reader struct {
	h    hierarchy
	proc procfs.FS
}

// read walks the hierarchy, picks the directories that belong to a container,
// and reads every thread of every process in them.
func (r reader) read() walk {
	w := walk{threads: map[int]thread{}, processes: map[int]string{}, containers: map[string]int{}}
	seen := map[string]bool{}
	_ = filepath.WalkDir(r.h.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil // a cgroup that vanished during the walk is not an error
		}
		scope, ok := containerScope(strings.TrimPrefix(path, r.h.root))
		if !ok {
			return nil
		}
		if !seen[scope] {
			seen[scope] = true
			w.containers[location(scope)]++
		}
		for _, pid := range readPids(filepath.Join(path, "cgroup.procs")) {
			threads, err := r.proc.AllThreads(pid)
			if err != nil {
				continue // the process exited
			}
			w.processes[pid] = scope
			for _, t := range threads {
				st, err := t.Schedstat()
				if err != nil {
					continue
				}
				w.threads[t.PID] = thread{cgroup: scope, wait: st.WaitingNanoseconds, run: st.RunningNanoseconds}
			}
		}
		return nil
	})
	return w
}

// cgroupOf returns the container scope a live (or not yet reaped) task belongs
// to, from /proc/<pid>/cgroup. ok is false if the task is gone or in no container.
func (r reader) cgroupOf(pid int) (scope string, ok bool) {
	p, err := r.proc.Proc(pid)
	if err != nil {
		return "", false
	}
	cgs, err := p.Cgroups()
	if err != nil {
		return "", false
	}
	for _, c := range cgs {
		if (r.h.version == "v2" && c.HierarchyID == 0) || (r.h.version == "v1" && slices.Contains(c.Controllers, "cpuacct")) {
			return containerScope(c.Path)
		}
	}
	return "", false
}

// exists reports whether a container's cgroup directory is still there.
func (r reader) exists(scope string) bool {
	_, err := os.Stat(filepath.Join(r.h.root, scope))
	return err == nil
}

func readPids(path string) []int {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var pids []int
	s := bufio.NewScanner(f)
	for s.Scan() {
		if pid, err := strconv.Atoi(strings.TrimSpace(s.Text())); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}
