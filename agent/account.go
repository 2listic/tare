package main

import (
	"sync"
	"time"
)

// pair is a container's accounted CPU wait and CPU run time, in nanoseconds.
type pair struct{ wait, run uint64 }

func (p *pair) add(wait, run uint64) { p.wait += wait; p.run += run }

// placeholder stands in for a thread that has left /proc but whose exit record
// has not arrived yet: its last read value, so the total does not dip.
type placeholder struct {
	thread
	since time.Time // when the thread vanished
}

// pendingTimeout is how long a placeholder waits for its exit record before it
// becomes final. Wall-clock, not a number of reads: with several scrapers or a
// short scrape interval, reads come milliseconds apart while the record is still
// in flight, and it would then be added on top of the finalised placeholder.
const pendingTimeout = 30 * time.Second

// tombstoneTTL is how long a finalised placeholder is remembered, so that an
// exit record arriving after pendingTimeout adds only what the placeholder did
// not already count.
const tombstoneTTL = 10 * time.Minute

// exitRecord is what an exit notification tells about a task.
type exitRecord struct {
	tid, ppid, tgid uint32
	wait, run       uint64 // final values, ns
	layout          string // which struct taskstats layout it was decoded with
}

// started is the container a process was in when it exec'd (process connector).
type started struct {
	scope string
	read  uint64 // the read during which it exec'd
}

// stats are the agent's self-checks, exported as tare_agent_* counters.
type stats struct {
	reads, exits, unattributed, outside, neverReceived, layoutUnknown, drops uint64
	execs, execsInContainers, execDrops, placedByExec                        uint64
	listening                                                                bool // exit listener registered and receiving
	layouts                                                                  map[string]uint64
	containers                                                               map[string]int
}

// account merges the two sources of CPU wait (complete run-queue accounting):
// the live threads read on every scrape, and the final values of exited tasks.
// The exit listener and the scrapes run concurrently, so every method locks mu.
type account struct {
	mu        sync.Mutex
	live      map[int]thread      // tid -> last read
	procs     map[int]string      // tgid -> container, last read
	prevProcs map[int]string      // the read before: exit attribution looks one read back
	exited    map[uint32]bool     // tids whose exit record arrived since the last read
	pending   map[int]placeholder // tid -> placeholder (left /proc, no exit record yet)
	atExec    map[int]started     // tgid -> container at exec, for tasks no read saw
	finalized map[int]placeholder // tid -> placeholder made final without its record
	saved     map[string]*pair    // container -> final values of exited tasks
	st        stats
	now       func() time.Time
}

func newAccount() *account {
	return &account{
		live: map[int]thread{}, procs: map[int]string{}, prevProcs: map[int]string{},
		exited: map[uint32]bool{}, pending: map[int]placeholder{}, finalized: map[int]placeholder{},
		atExec: map[int]started{}, saved: map[string]*pair{},
		st:  stats{layouts: map[string]uint64{}, containers: map[string]int{}},
		now: time.Now,
	}
}

// apply folds one read into the state and returns the CPU wait and run time
// of every container. exists tells whether a container's cgroup is still there.
func (a *account) apply(w walk, exists func(scope string) bool) map[string]pair {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.st.reads++
	now := a.now()
	a.st.containers = w.containers
	a.prevProcs, a.procs = a.procs, w.processes

	// Threads whose exit record arrived. A thread can still be listed in /proc
	// after its record (a zombie, or a read racing the exit): it stays marked as
	// long as it is visible, or its last read value would be counted a second
	// time when it finally disappears.
	done := a.exited
	a.exited = map[uint32]bool{}
	for tid := range done {
		if _, still := w.threads[int(tid)]; still {
			a.exited[tid] = true
		}
	}
	// Threads that vanished since the last read: the exit record usually arrived
	// already (done); if not, keep the last read value as a placeholder.
	for tid, t := range a.live {
		if _, still := w.threads[tid]; !still && !done[uint32(tid)] {
			a.pending[tid] = placeholder{thread: t, since: now}
		}
	}
	a.live = w.threads

	// A thread that missed one read (a /proc read that failed, a read racing a
	// cgroup move) and is back: it is live again, and its placeholder would
	// count it a second time. Only the same thread: same container, counters not
	// lower (a reused tid starts from zero).
	for tid, p := range a.pending {
		if t, back := w.threads[tid]; back && sameThread(p.thread, t) {
			delete(a.pending, tid)
		}
	}

	// A placeholder whose exit record never came (dropped) becomes final.
	for tid, p := range a.pending {
		if now.Sub(p.since) > pendingTimeout {
			a.savedFor(p.cgroup).add(p.wait, p.run)
			delete(a.pending, tid)
			a.finalized[tid] = placeholder{thread: p.thread, since: now}
			a.st.neverReceived++
		}
	}
	for tid, p := range a.finalized {
		if now.Sub(p.since) > tombstoneTTL {
			delete(a.finalized, tid)
		}
	}
	// An exec is kept until two reads have passed: a process that lives longer
	// is in the reads, and the PID may meanwhile belong to someone else.
	for tgid, s := range a.atExec {
		if s.read+2 < a.st.reads {
			delete(a.atExec, tgid)
		}
	}

	totals := map[string]pair{}
	for scope, s := range a.saved {
		totals[scope] = *s
	}
	for _, p := range a.pending {
		t := totals[p.cgroup]
		t.add(p.wait, p.run)
		totals[p.cgroup] = t
	}
	alive := map[string]bool{}
	for tid, t := range w.threads {
		alive[t.cgroup] = true
		if done[uint32(tid)] {
			continue // its exit record already holds its final value
		}
		tot := totals[t.cgroup]
		tot.add(t.wait, t.run)
		totals[t.cgroup] = tot
	}
	// Containers that are gone: stop exporting them.
	for scope := range a.saved {
		if !alive[scope] && !exists(scope) {
			delete(a.saved, scope)
			delete(totals, scope)
		}
	}
	return totals
}

func sameThread(was, now thread) bool {
	return was.cgroup == now.cgroup && now.wait >= was.wait && now.run >= was.run
}

func (a *account) savedFor(scope string) *pair {
	p, ok := a.saved[scope]
	if !ok {
		p = &pair{}
		a.saved[scope] = p
	}
	return p
}

// exit records one exit notification. cgroupOf reads a task's container from
// /proc (it may already be gone). Attribution, in order: where the thread was
// last seen; its process; the container its process exec'd in; its parent. An
// exiting task can report a cgroup outside any container (cgroup v1, kernel
// 5.10), so from the task itself only a container cgroup is accepted, otherwise
// the parent decides.
func (a *account) exit(r exitRecord, cgroupOf func(pid int) (string, bool)) {
	tid, tgid, ppid := int(r.tid), int(r.tgid), int(r.ppid)

	a.mu.Lock()
	a.st.exits++
	a.st.layouts[r.layout]++
	scope, found := a.lookup(tid, tgid)
	if !found {
		// Before /proc and the parent: a task started from outside its container
		// (an exec probe, parent the runtime's shim) is usually reaped by now,
		// and its parent would say "host".
		if scope, found = a.execdAs(tid, tgid); found {
			a.st.placedByExec++
		}
	}
	a.mu.Unlock()

	if !found {
		if s, ok := cgroupOf(tid); ok {
			scope, found = s, true
		} else if tgid != 0 {
			scope, found = cgroupOf(tgid)
		}
	}
	parent := ""
	if !found && ppid != 0 {
		a.mu.Lock()
		scope, found = a.processOf(ppid)
		a.mu.Unlock()
		if !found {
			parent, _ = cgroupOf(ppid)
			scope, found = containerScope(parent)
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if !found {
		// Outside: the task, or the parent it inherited its cgroup from, is
		// readable and in no container (host processes). Unattributed: both were
		// gone before anyone could tell.
		if s, _ := cgroupOf(tid); s != "" || parent != "" {
			a.st.outside++
		} else {
			a.st.unattributed++
		}
		return
	}
	// A record arriving after its placeholder was made final adds only what the
	// placeholder did not already count.
	if f, late := a.finalized[tid]; late {
		delete(a.finalized, tid)
		if final := (thread{scope, r.wait, r.run}); sameThread(f.thread, final) {
			a.savedFor(scope).add(r.wait-f.wait, r.run-f.run)
			a.exited[r.tid] = true
			return
		}
	}
	a.savedFor(scope).add(r.wait, r.run)
	delete(a.pending, tid) // the final value replaces the placeholder
	a.exited[r.tid] = true
}

// lookup finds a task in what the reads already know. Caller holds mu.
func (a *account) lookup(tid, tgid int) (string, bool) {
	if t, ok := a.live[tid]; ok {
		return t.cgroup, true
	}
	if p, ok := a.pending[tid]; ok {
		return p.cgroup, true
	}
	if p, ok := a.finalized[tid]; ok {
		return p.cgroup, true
	}
	if s, ok := a.processOf(tgid); ok && tgid != 0 {
		return s, true
	}
	return a.processOf(tid)
}

// execdAs finds the container a task's process exec'd in. A thread is found by
// its process; on kernels without ac_tgid (4.18) tgid is 0 and the main thread by
// its own id. Caller holds mu.
func (a *account) execdAs(tid, tgid int) (string, bool) {
	if s, ok := a.atExec[tgid]; ok && tgid != 0 {
		return s.scope, true
	}
	s, ok := a.atExec[tid]
	return s.scope, ok
}

// execd records an exec announced by the process connector: the process's
// container, read from /proc while it runs. A process that exec'd outside any
// container clears what its PID meant before.
func (a *account) execd(tgid int, scope string, inContainer bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.st.execs++
	if !inContainer {
		delete(a.atExec, tgid)
		return
	}
	a.st.execsInContainers++
	a.atExec[tgid] = started{scope: scope, read: a.st.reads}
}

// forked forgets a PID that a new process now holds.
func (a *account) forked(tgid int) {
	a.mu.Lock()
	delete(a.atExec, tgid)
	a.mu.Unlock()
}

// processOf finds a process in the last two reads. Caller holds mu.
func (a *account) processOf(pid int) (string, bool) {
	if s, ok := a.procs[pid]; ok {
		return s, true
	}
	s, ok := a.prevProcs[pid]
	return s, ok
}

// snapshot returns a copy of the self-checks.
func (a *account) snapshot() stats {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.st
	s.layouts = map[string]uint64{}
	for k, v := range a.st.layouts {
		s.layouts[k] = v
	}
	s.containers = map[string]int{}
	for k, v := range a.st.containers {
		s.containers[k] = v
	}
	return s
}

// layoutUnknown and drop are counted by the exit listener.
func (a *account) layoutUnknown() { a.mu.Lock(); a.st.exits++; a.st.layoutUnknown++; a.mu.Unlock() }
func (a *account) drop()          { a.mu.Lock(); a.st.drops++; a.mu.Unlock() }

// execDrop is counted by the exec listener.
func (a *account) execDrop() { a.mu.Lock(); a.st.execDrops++; a.mu.Unlock() }

// setListening records whether the exit listener is registered and receiving.
func (a *account) setListening(up bool) { a.mu.Lock(); a.st.listening = up; a.mu.Unlock() }

func (a *account) listening() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.st.listening }
