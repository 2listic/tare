package main

import (
	"testing"
	"time"
)

const (
	appCg   = "/kubepods.slice/p.slice/cri-containerd-a.scope"
	otherCg = "/kubepods.slice/q.slice/cri-containerd-b.scope"
)

// read builds one walk: thread id -> (container, wait, run); the process of each
// thread is the thread itself unless listed in procs.
func read(threads map[int]thread, procs map[int]string) walk {
	w := walk{threads: threads, processes: map[int]string{}, containers: map[string]int{}}
	for tid, t := range threads {
		w.processes[tid] = t.cgroup
	}
	for pid, cg := range procs {
		w.processes[pid] = cg
	}
	return w
}

func always(string) bool { return true }

// noProc is a /proc where every task is gone.
func noProc(int) (string, bool) { return "", false }

func TestLiveThreadsAreSummedPerContainer(t *testing.T) {
	a := newAccount()
	got := a.apply(read(map[int]thread{
		1: {appCg, 10, 100}, 2: {appCg, 5, 50}, 3: {otherCg, 1, 1},
	}, nil), always)
	if got[appCg] != (pair{15, 150}) || got[otherCg] != (pair{1, 1}) {
		t.Errorf("totals = %v", got)
	}
}

// The exit record holds the final value; the thread must not count twice even
// while it is still listed in /proc.
func TestExitedThreadIsNotCountedTwice(t *testing.T) {
	a := newAccount()
	a.apply(read(map[int]thread{1: {appCg, 10, 100}, 2: {appCg, 5, 50}}, nil), always)
	a.exit(exitRecord{tid: 2, wait: 7, run: 60}, noProc)
	// thread 2 still visible in this read (a zombie, or the read raced the exit)
	got := a.apply(read(map[int]thread{1: {appCg, 12, 110}, 2: {appCg, 7, 60}}, nil), always)
	if want := (pair{12 + 7, 110 + 60}); got[appCg] != want {
		t.Errorf("got %v, want %v", got[appCg], want)
	}
	got = a.apply(read(map[int]thread{1: {appCg, 12, 110}}, nil), always)
	if want := (pair{12 + 7, 110 + 60}); got[appCg] != want {
		t.Errorf("after the thread left /proc: got %v, want %v", got[appCg], want)
	}
}

// A thread that leaves /proc before its exit record arrives keeps its last read
// value until the record replaces it: the counter never dips.
func TestPlaceholderUntilTheExitRecordArrives(t *testing.T) {
	a := newAccount()
	a.apply(read(map[int]thread{1: {appCg, 10, 100}, 2: {appCg, 5, 50}}, nil), always)
	got := a.apply(read(map[int]thread{1: {appCg, 10, 100}}, nil), always) // 2 gone, no record yet
	if want := (pair{15, 150}); got[appCg] != want {
		t.Fatalf("with placeholder: got %v, want %v", got[appCg], want)
	}
	a.exit(exitRecord{tid: 2, wait: 6, run: 55}, noProc) // final value, a bit more than last read
	got = a.apply(read(map[int]thread{1: {appCg, 10, 100}}, nil), always)
	if want := (pair{16, 155}); got[appCg] != want {
		t.Errorf("after the record: got %v, want %v (placeholder replaced, not added)", got[appCg], want)
	}
}

// clock is a fake time source; advance moves it on.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }
func withClock(a *account) (*account, *clock) {
	c := &clock{t: time.Unix(1e9, 0)}
	a.now = c.now
	return a, c
}

func TestPlaceholderBecomesFinalWhenTheRecordNeverComes(t *testing.T) {
	a, c := withClock(newAccount())
	a.apply(read(map[int]thread{1: {appCg, 10, 100}, 2: {appCg, 5, 50}}, nil), always)
	var got map[string]pair
	for i := 0; i < 4; i++ {
		c.advance(15 * time.Second)
		got = a.apply(read(map[int]thread{1: {appCg, 10, 100}}, nil), always)
	}
	if want := (pair{15, 150}); got[appCg] != want {
		t.Errorf("got %v, want %v", got[appCg], want)
	}
	if s := a.snapshot(); s.neverReceived != 1 {
		t.Errorf("neverReceived = %d, want 1", s.neverReceived)
	}
}

// A process that lived entirely between two reads is placed through its parent.
func TestShortLivedProcessIsAttributedToItsParent(t *testing.T) {
	a := newAccount()
	a.apply(read(map[int]thread{1: {appCg, 0, 0}}, nil), always) // the parent, pid 1
	// the child reports a cgroup outside any container (cgroup v1, kernel 5.10)
	cgroupOf := func(pid int) (string, bool) {
		if pid == 50 {
			return "/", false
		}
		return "", false
	}
	a.exit(exitRecord{tid: 50, ppid: 1, wait: 3, run: 9}, cgroupOf)
	got := a.apply(read(map[int]thread{1: {appCg, 0, 0}}, nil), always)
	if want := (pair{3, 9}); got[appCg] != want {
		t.Errorf("got %v, want %v", got[appCg], want)
	}
}

func TestHostAndVanishedTasksAreCountedApart(t *testing.T) {
	a := newAccount()
	a.apply(read(map[int]thread{}, nil), always)
	host := func(int) (string, bool) { return "/system.slice/sshd.service", false }
	a.exit(exitRecord{tid: 70, wait: 1, run: 1}, host)
	a.exit(exitRecord{tid: 71, wait: 1, run: 1}, noProc)
	s := a.snapshot()
	if s.outside != 1 || s.unattributed != 1 || s.exits != 2 {
		t.Errorf("outside=%d unattributed=%d exits=%d, want 1 1 2", s.outside, s.unattributed, s.exits)
	}
}

func TestDeletedContainersAreDropped(t *testing.T) {
	a := newAccount()
	a.apply(read(map[int]thread{1: {appCg, 10, 100}}, nil), always)
	a.exit(exitRecord{tid: 1, wait: 10, run: 100}, noProc)
	gone := func(string) bool { return false }
	got := a.apply(read(map[int]thread{}, nil), gone)
	if _, ok := got[appCg]; ok {
		t.Errorf("a deleted container is still exported: %v", got)
	}
}

// A short-lived host process is gone by the time its record is read, but its
// parent is still there and in no container: outside, not unattributed.
func TestHostChildIsOutsideThroughItsParent(t *testing.T) {
	a := newAccount()
	a.apply(read(map[int]thread{}, nil), always)
	cgroupOf := func(pid int) (string, bool) {
		if pid == 1 {
			return "/user.slice/session-2.scope", false
		}
		return "", false // the child itself is gone
	}
	a.exit(exitRecord{tid: 80, ppid: 1, wait: 1, run: 1}, cgroupOf)
	if s := a.snapshot(); s.outside != 1 || s.unattributed != 0 {
		t.Errorf("outside=%d unattributed=%d, want 1 0", s.outside, s.unattributed)
	}
}

// An exec probe: started by the runtime, so its parent is the shim on the host.
// By the time its exit record is handled the shim has reaped it. Without its
// exec event it would be counted as a host process (the test above).
func shimParent(pid int) (string, bool) {
	if pid == 9 {
		return "/system.slice/containerd.service", false // the shim
	}
	return "", false // the probe itself is gone
}

func TestExecProbeIsPlacedThroughItsExec(t *testing.T) {
	a := newAccount()
	a.apply(read(map[int]thread{}, nil), always)
	a.execd(500, appCg, true)
	a.exit(exitRecord{tid: 501, tgid: 500, ppid: 9, wait: 1, run: 4}, shimParent) // a thread of it
	a.exit(exitRecord{tid: 500, tgid: 500, ppid: 9, wait: 2, run: 5}, shimParent)
	got := a.apply(read(map[int]thread{}, nil), always)
	if want := (pair{3, 9}); got[appCg] != want {
		t.Errorf("got %v, want %v", got[appCg], want)
	}
	if s := a.snapshot(); s.placedByExec != 2 || s.outside != 0 {
		t.Errorf("placedByExec=%d outside=%d, want 2 0", s.placedByExec, s.outside)
	}
}

// Kernels without ac_tgid (4.18) report tgid 0: the main thread is found by its id.
func TestExecProbeWithoutTgid(t *testing.T) {
	a := newAccount()
	a.apply(read(map[int]thread{}, nil), always)
	a.execd(500, appCg, true)
	a.exit(exitRecord{tid: 500, ppid: 9, wait: 2, run: 5}, shimParent)
	if s := a.snapshot(); s.placedByExec != 1 {
		t.Errorf("placedByExec=%d, want 1", s.placedByExec)
	}
}

// What an exec says about a PID stops being true when the PID is reused, the
// process execs outside any container, or two reads have passed.
func TestExecIsForgotten(t *testing.T) {
	tests := []struct {
		name   string
		forget func(a *account)
	}{
		{"a new process took the PID", func(a *account) { a.forked(500) }},
		{"it exec'd outside any container", func(a *account) { a.execd(500, "/system.slice/cron.service", false) }},
		{"two reads passed", func(a *account) {
			for i := 0; i < 3; i++ {
				a.apply(read(map[int]thread{}, nil), always)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newAccount()
			a.apply(read(map[int]thread{}, nil), always)
			a.execd(500, appCg, true)
			tt.forget(a)
			a.exit(exitRecord{tid: 500, tgid: 500, ppid: 9, wait: 2, run: 5}, shimParent)
			if s := a.snapshot(); s.placedByExec != 0 || s.outside != 1 {
				t.Errorf("placedByExec=%d outside=%d, want 0 1", s.placedByExec, s.outside)
			}
		})
	}
}

// Where a read saw the task, the read decides: the exec is only a fallback.
func TestReadsComeBeforeExecs(t *testing.T) {
	a := newAccount()
	a.apply(read(map[int]thread{500: {otherCg, 0, 0}}, nil), always)
	a.execd(500, appCg, true)
	a.exit(exitRecord{tid: 500, tgid: 500, ppid: 9, wait: 2, run: 5}, shimParent)
	got := a.apply(read(map[int]thread{}, nil), always)
	if got[otherCg] != (pair{2, 5}) || got[appCg] != (pair{}) {
		t.Errorf("got %v", got)
	}
	if s := a.snapshot(); s.placedByExec != 0 {
		t.Errorf("placedByExec=%d, want 0", s.placedByExec)
	}
}

// A thread missing from one read and back in the next is live, not also a
// placeholder: counted once, and never made final behind its back.
func TestThreadThatMissesOneReadIsNotCountedTwice(t *testing.T) {
	a, c := withClock(newAccount())
	a.apply(read(map[int]thread{1: {appCg, 1000, 5000}}, nil), always)
	a.apply(read(map[int]thread{}, nil), always) // missed
	var got map[string]pair
	for i := 0; i < 4; i++ {
		c.advance(15 * time.Second)
		got = a.apply(read(map[int]thread{1: {appCg, 1001, 5001}}, nil), always)
	}
	if want := (pair{1001, 5001}); got[appCg] != want {
		t.Errorf("got %v, want %v", got[appCg], want)
	}
}

// A tid reused by a new thread is not the vanished one: the placeholder stays,
// or the total would dip by the old thread's value.
func TestReusedTidKeepsThePlaceholder(t *testing.T) {
	a := newAccount()
	a.apply(read(map[int]thread{1: {appCg, 1000, 5000}}, nil), always)
	a.apply(read(map[int]thread{}, nil), always)
	got := a.apply(read(map[int]thread{1: {appCg, 3, 7}}, nil), always) // new thread, same tid
	if want := (pair{1003, 5007}); got[appCg] != want {
		t.Errorf("got %v, want %v", got[appCg], want)
	}
}

// Reads milliseconds apart (several scrapers) must not make a placeholder final
// while its exit record is still in flight.
func TestRapidReadsDoNotFinalisePlaceholders(t *testing.T) {
	a, c := withClock(newAccount())
	a.apply(read(map[int]thread{1: {appCg, 10, 100}, 2: {appCg, 5, 50}}, nil), always)
	for i := 0; i < 20; i++ {
		c.advance(5 * time.Millisecond)
		a.apply(read(map[int]thread{1: {appCg, 10, 100}}, nil), always)
	}
	a.exit(exitRecord{tid: 2, wait: 6, run: 55}, noProc)
	got := a.apply(read(map[int]thread{1: {appCg, 10, 100}}, nil), always)
	if want := (pair{16, 155}); got[appCg] != want {
		t.Errorf("got %v, want %v", got[appCg], want)
	}
	if s := a.snapshot(); s.neverReceived != 0 {
		t.Errorf("neverReceived = %d, want 0", s.neverReceived)
	}
}

// A record that arrives after its placeholder was made final adds only the
// difference.
func TestLateRecordAddsOnlyTheDifference(t *testing.T) {
	a, c := withClock(newAccount())
	a.apply(read(map[int]thread{1: {appCg, 10, 100}, 2: {appCg, 5, 50}}, nil), always)
	a.apply(read(map[int]thread{1: {appCg, 10, 100}}, nil), always)
	c.advance(pendingTimeout + time.Second)
	a.apply(read(map[int]thread{1: {appCg, 10, 100}}, nil), always) // placeholder final
	a.exit(exitRecord{tid: 2, wait: 6, run: 55}, noProc)
	got := a.apply(read(map[int]thread{1: {appCg, 10, 100}}, nil), always)
	if want := (pair{16, 155}); got[appCg] != want {
		t.Errorf("got %v, want %v", got[appCg], want)
	}
}
