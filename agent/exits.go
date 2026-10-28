package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/mdlayher/genetlink"
	"github.com/mdlayher/netlink"
	"golang.org/x/sys/unix"
)

// layout is where the fields this agent needs sit in a struct taskstats.
// cpu_delay_total (the run-queue wait) is at offset 24 in every layout seen.
type layout struct {
	name                 string
	run, pid, ppid, tgid int // offsets of cpu_run_real_total, ac_pid, ac_ppid, ac_tgid
}

// layouts seen in the wild. The struct's version field does not tell them apart:
// RHEL 10's 6.12 kernel reports version 15 like upstream but inserts max/min delay
// fields after each *_delay_total, moving everything after cpu_delay_total by 48
// bytes (96 for ac_tgid). Old kernels (version 9, 4.18) end before ac_tgid.
var layouts = []layout{
	{name: "upstream", run: 64, pid: 128, ppid: 132, tgid: 368},
	{name: "rhel10", run: 112, pid: 176, ppid: 180, tgid: 464},
}

const offWait = 24 // cpu_delay_total

// decode reads an exit record. The layout is recognised by finding the thread id,
// which the kernel also sends in its own attribute, at the layout's ac_pid offset.
// ok is false when no known layout matches: the record is counted, not guessed.
func decode(tid uint32, b []byte) (r exitRecord, ok bool) {
	u32 := func(off int) uint32 { return binary.NativeEndian.Uint32(b[off:]) }
	u64 := func(off int) uint64 { return binary.NativeEndian.Uint64(b[off:]) }
	for _, l := range layouts {
		if len(b) < l.ppid+4 || len(b) < l.run+8 || u32(l.pid) != tid {
			continue
		}
		r = exitRecord{tid: tid, ppid: u32(l.ppid), wait: u64(offWait), run: u64(l.run), layout: l.name}
		if len(b) >= l.tgid+4 {
			r.tgid = u32(l.tgid)
		}
		return r, true
	}
	return exitRecord{}, false
}

// listenExits subscribes to the kernel's taskstats exit notifications on every
// online CPU and feeds each record to the account. It returns when registration
// is refused or the socket fails; lost records (ENOBUFS) are counted, not fatal.
func listenExits(a *account, cgroupOf func(pid int) (string, bool)) error {
	c, err := genetlink.Dial(nil)
	if err != nil {
		return fmt.Errorf("netlink: %w", err)
	}
	defer c.Close()

	// SO_RCVBUF is silently capped at net.core.rmem_max (a few hundred records);
	// SO_RCVBUFFORCE (CAP_NET_ADMIN) is not.
	if rc, err := c.SyscallConn(); err == nil {
		_ = rc.Control(func(fd uintptr) {
			if unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUFFORCE, 64<<20) != nil {
				_ = c.SetReadBuffer(64 << 20)
			}
		})
	}

	fam, err := c.GetFamily(unix.TASKSTATS_GENL_NAME)
	if err != nil {
		return fmt.Errorf("taskstats family: %w", err)
	}
	cpus := onlineCPUs()
	ae := netlink.NewAttributeEncoder()
	ae.String(unix.TASKSTATS_CMD_ATTR_REGISTER_CPUMASK, cpus)
	data, err := ae.Encode()
	if err != nil {
		return err
	}
	req := genetlink.Message{Header: genetlink.Header{Command: unix.TASKSTATS_CMD_GET, Version: fam.Version}, Data: data}
	if _, err := c.Send(req, fam.ID, netlink.Request|netlink.Acknowledge); err != nil {
		return fmt.Errorf("register for exits: %w", err)
	}
	// A refused registration (EPERM without CAP_NET_ADMIN) is not an error of
	// Send: it arrives on the next Receive, as the reply to the request. The
	// listener counts as up only once a Receive has succeeded (the ack, or a
	// record), and goes down with any error other than lost records.
	log.Printf("taskstats family %d, registration sent for cpus %s", fam.ID, cpus)
	for {
		msgs, _, err := c.Receive()
		if errors.Is(err, unix.ENOBUFS) {
			a.drop() // the receive buffer overflowed: exit records were lost
			log.Printf("netlink receive: %v", err)
			continue
		}
		if err != nil {
			return fmt.Errorf("taskstats: %w", err)
		}
		a.setListening(true)
		for _, m := range msgs {
			if m.Header.Command != unix.TASKSTATS_CMD_NEW {
				continue
			}
			ad, err := netlink.NewAttributeDecoder(m.Data)
			if err != nil {
				continue
			}
			for ad.Next() {
				if ad.Type() != unix.TASKSTATS_TYPE_AGGR_PID {
					continue // AGGR_TGID would count the threads of a process twice
				}
				var tid uint32
				var raw []byte
				ad.Nested(func(nad *netlink.AttributeDecoder) error {
					for nad.Next() {
						switch nad.Type() {
						case unix.TASKSTATS_TYPE_PID:
							tid = nad.Uint32()
						case unix.TASKSTATS_TYPE_STATS:
							raw = nad.Bytes()
						}
					}
					return nil
				})
				if tid == 0 || raw == nil {
					continue
				}
				if r, ok := decode(tid, raw); ok {
					a.exit(r, cgroupOf)
				} else {
					a.layoutUnknown()
				}
			}
		}
	}
}

// onlineCPUs is the cpu list to register, e.g. "0-3". Offline CPUs cannot be
// registered.
func onlineCPUs() string {
	if b, err := os.ReadFile("/sys/devices/system/cpu/online"); err == nil {
		return strings.TrimSpace(string(b))
	}
	return "0"
}
