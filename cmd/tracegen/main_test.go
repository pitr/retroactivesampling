package main

import (
	"context"
	"encoding/binary"
	"testing"
)

func TestNextPow2(t *testing.T) {
	cases := []struct{ in, want uint64 }{
		{0, 1},
		{1, 1},
		{2, 2},
		{3, 4},
		{1023, 1024},
		{1024, 1024},
		{1025, 2048},
	}
	for _, c := range cases {
		if got := nextPow2(c.in); got != c.want {
			t.Errorf("nextPow2(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestSeqIDGen(t *testing.T) {
	g := &seqIDGen{}
	ctx := context.Background()

	tid0, sid0 := g.NewIDs(ctx)
	tid1, sid1 := g.NewIDs(ctx)
	tid2, _ := g.NewIDs(ctx)

	for i := range 8 {
		if tid0[i] != 0 {
			t.Errorf("tid0[%d] = %d, want 0", i, tid0[i])
		}
	}
	if binary.BigEndian.Uint64(tid0[8:]) != 0 {
		t.Error("first seqID should be 0")
	}
	if binary.BigEndian.Uint64(tid1[8:]) != 1 {
		t.Error("second seqID should be 1")
	}
	if binary.BigEndian.Uint64(tid2[8:]) != 2 {
		t.Error("third seqID should be 2")
	}
	if sid0 == sid1 {
		t.Error("span IDs should differ between calls")
	}
}

func TestNewRing(t *testing.T) {
	// rate=10, timeout=30 → 10*30*4=1200 → nextPow2 → 2048
	r := newRing(10, 30)
	if len(r.slots) != 2048 {
		t.Errorf("ring size = %d, want 2048", len(r.slots))
	}
	if r.mask != 2047 {
		t.Errorf("mask = %d, want 2047", r.mask)
	}

	// minimum size: rate=1, timeout=1 → 4 → but minimum is 1024
	r2 := newRing(1, 1)
	if len(r2.slots) != 1024 {
		t.Errorf("minimum ring size = %d, want 1024", len(r2.slots))
	}
}
