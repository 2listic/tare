package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"syscall"

	"golang.org/x/sys/unix"
)

// The kernel's process connector (cn_proc) announces every fork and exec on the
// node. The agent uses it for tasks started from outside their container: an exec
// probe runs in the container, but its parent is the runtime's shim, on the host.
// The task lives a few milliseconds, no read sees it, and by the time its exit
// record is handled the shim has usually reaped it: /proc no longer tells its
// cgroup, and its parent says "host". Read at exec, while the task runs, the
// cgroup is still there.

const (
	cnIdxProc = 1 // CN_IDX_PROC
	cnValProc = 1 // CN_VAL_PROC

	procCnMcastListen = 1 // PROC_CN_MCAST_LISTEN

	procEventFork = 0x00000001 // PROC_EVENT_FORK
	procEventExec = 0x00000002 // PROC_EVENT_EXEC

	cnMsgLen     = 20 // struct cn_msg: cb_id{idx, val}, seq, ack, len (u16), flags (u16)
	procEventHdr = 16 // struct proc_event: what, cpu, timestamp_ns
)

// procEvent is the part of a cn_proc event the agent uses.
type procEvent struct {
	what      uint32
	pid, tgid uint32 // exec: the process; fork: the child
}

// decodeProcEvent reads one connector message (struct cn_msg + struct proc_event).
// ok is false for anything that is not a fork or an exec of the process connector.
func decodeProcEvent(b []byte) (e procEvent, ok bool) {
	u32 := func(off int) uint32 { return binary.NativeEndian.Uint32(b[off:]) }
	if len(b) < cnMsgLen+procEventHdr || u32(0) != cnIdxProc || u32(4) != cnValProc {
		return procEvent{}, false
	}
	data := cnMsgLen + procEventHdr
	switch e.what = u32(cnMsgLen); e.what {
	case procEventExec: // process_pid, process_tgid
		if len(b) < data+8 {
			return procEvent{}, false
		}
		e.pid, e.tgid = u32(data), u32(data+4)
	case procEventFork: // parent_pid, parent_tgid, child_pid, child_tgid
		if len(b) < data+16 {
			return procEvent{}, false
		}
		e.pid, e.tgid = u32(data+8), u32(data+12)
	default:
		return procEvent{}, false
	}
	return e, true
}

// listenExecs subscribes to the process connector and records the container of
// every task that execs. It runs until the socket fails. Like the exit listener it
// needs CAP_NET_ADMIN and the host's network and PID namespaces.
func listenExecs(a *account, cgroupOf func(pid int) (string, bool)) error {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.NETLINK_CONNECTOR)
	if err != nil {
		return fmt.Errorf("netlink connector: %w", err)
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: cnIdxProc}); err != nil {
		return fmt.Errorf("bind to the process connector: %w", err)
	}
	// every fork on the node is an event: the same 64 MB as the exit listener
	if unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUFFORCE, 64<<20) != nil {
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, 64<<20)
	}

	// nlmsghdr + cn_msg + PROC_CN_MCAST_LISTEN
	const size = unix.NLMSG_HDRLEN + cnMsgLen + 4
	req := make([]byte, size)
	put32 := func(off int, v uint32) { binary.NativeEndian.PutUint32(req[off:], v) }
	put32(0, size)
	binary.NativeEndian.PutUint16(req[4:], unix.NLMSG_DONE)
	h := unix.NLMSG_HDRLEN
	put32(h, cnIdxProc)
	put32(h+4, cnValProc)
	binary.NativeEndian.PutUint16(req[h+16:], 4)
	put32(h+cnMsgLen, procCnMcastListen)
	if err := unix.Sendto(fd, req, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fmt.Errorf("subscribe to process events: %w", err)
	}
	log.Printf("process connector: listening for execs")

	buf := make([]byte, 64<<10)
	for {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if errors.Is(err, unix.ENOBUFS) {
				a.execDrop() // events were lost: those tasks fall back to the exit-time lookup
				continue
			}
			return fmt.Errorf("receive process events: %w", err)
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			continue
		}
		for _, m := range msgs {
			e, ok := decodeProcEvent(m.Data)
			if !ok {
				continue
			}
			switch {
			case e.what == procEventFork && e.pid == e.tgid:
				// a new process (not a thread): whatever an earlier holder of
				// this PID exec'd into is no longer true
				a.forked(int(e.tgid))
			case e.what == procEventExec:
				scope, inContainer := cgroupOf(int(e.pid))
				a.execd(int(e.tgid), scope, inContainer)
			}
		}
	}
}
