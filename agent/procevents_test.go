package main

import (
	"encoding/binary"
	"testing"
)

// connectorMsg builds a struct cn_msg carrying a struct proc_event, the way the
// kernel sends it.
func connectorMsg(idx, what uint32, data ...uint32) []byte {
	b := make([]byte, cnMsgLen+procEventHdr+4*len(data))
	binary.NativeEndian.PutUint32(b[0:], idx)
	binary.NativeEndian.PutUint32(b[4:], cnValProc)
	binary.NativeEndian.PutUint16(b[16:], uint16(procEventHdr+4*len(data)))
	binary.NativeEndian.PutUint32(b[cnMsgLen:], what)
	for i, v := range data {
		binary.NativeEndian.PutUint32(b[cnMsgLen+procEventHdr+4*i:], v)
	}
	return b
}

func TestDecodeProcEvent(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
		want procEvent
		ok   bool
	}{
		{"exec", connectorMsg(cnIdxProc, procEventExec, 501, 500), procEvent{procEventExec, 501, 500}, true},
		{"fork: the child", connectorMsg(cnIdxProc, procEventFork, 7, 7, 500, 500), procEvent{procEventFork, 500, 500}, true},
		{"exit events are not used here", connectorMsg(cnIdxProc, 0x80000000, 500, 500, 0, 0), procEvent{}, false},
		{"another connector", connectorMsg(cnIdxProc+1, procEventExec, 501, 500), procEvent{}, false},
		{"truncated exec", connectorMsg(cnIdxProc, procEventExec, 501), procEvent{}, false},
		{"truncated fork", connectorMsg(cnIdxProc, procEventFork, 7, 7, 500), procEvent{}, false},
		{"too short for a header", make([]byte, 10), procEvent{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := decodeProcEvent(tt.raw)
			if ok != tt.ok || got != tt.want {
				t.Errorf("got %+v, %v; want %+v, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}
