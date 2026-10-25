package main

import (
	"encoding/binary"
	"testing"
)

// record builds a struct taskstats of the given size with the fields this agent
// reads at the given offsets, the way the kernel would send it.
func record(size int, l layout, tid, ppid, tgid uint32, wait, run uint64) []byte {
	b := make([]byte, size)
	binary.NativeEndian.PutUint64(b[offWait:], wait)
	binary.NativeEndian.PutUint64(b[l.run:], run)
	binary.NativeEndian.PutUint32(b[l.pid:], tid)
	binary.NativeEndian.PutUint32(b[l.ppid:], ppid)
	if size >= l.tgid+4 {
		binary.NativeEndian.PutUint32(b[l.tgid:], tgid)
	}
	return b
}

// Sizes are those of real kernels: 432 bytes (version 14/15, upstream), 344 bytes
// (version 9, kernel 4.18), 560 bytes (version 15, RHEL 10 kernel 6.12).
func TestDecode(t *testing.T) {
	up, rhel := layouts[0], layouts[1]
	tests := []struct {
		name       string
		raw        []byte
		want       exitRecord
		wantLayout string
	}{
		{"upstream v15 (Ubuntu 6.8)", record(432, up, 100, 7, 99, 5000, 70000),
			exitRecord{tid: 100, ppid: 7, tgid: 99, wait: 5000, run: 70000}, "upstream"},
		{"v9 without ac_tgid (EL8 4.18)", record(344, up, 100, 7, 0, 5000, 70000),
			exitRecord{tid: 100, ppid: 7, tgid: 0, wait: 5000, run: 70000}, "upstream"},
		{"rhel10 v15 (AlmaLinux 10 6.12)", record(560, rhel, 100, 7, 99, 5000, 70000),
			exitRecord{tid: 100, ppid: 7, tgid: 99, wait: 5000, run: 70000}, "rhel10"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := decode(100, tt.raw)
			if !ok {
				t.Fatal("not decoded")
			}
			tt.want.layout = tt.wantLayout
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestDecodeUnknownLayoutIsNotGuessed(t *testing.T) {
	if _, ok := decode(100, make([]byte, 560)); ok {
		t.Error("a record with the thread id nowhere was decoded")
	}
	if _, ok := decode(100, make([]byte, 40)); ok {
		t.Error("a truncated record was decoded")
	}
}
