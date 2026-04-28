package main

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb    "go.opentelemetry.io/proto/otlp/trace/v1"
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

func TestListenerExport(t *testing.T) {
	ring := newRing(10, 30)
	l := &traceListener{ring: ring}

	// Trace with seqID=5, foreign upper bytes zero, 2 spans received
	var tid [16]byte
	binary.BigEndian.PutUint64(tid[8:], 5)

	req := &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			ScopeSpans: []*tracepb.ScopeSpans{{
				Spans: []*tracepb.Span{
					{TraceId: tid[:]},
					{TraceId: tid[:]},
				},
			}},
		}},
	}

	_, err := l.Export(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	slot := &ring.slots[5&ring.mask]
	if got := slot.spanCount.Load(); got != 2 {
		t.Errorf("spanCount = %d, want 2", got)
	}
	if slot.firstSeen.Load() == 0 {
		t.Error("firstSeen not set")
	}

	// Foreign span (upper 8 bytes non-zero) must be ignored
	var foreignTid [16]byte
	foreignTid[0] = 0xFF
	binary.BigEndian.PutUint64(foreignTid[8:], 99)
	req2 := &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			ScopeSpans: []*tracepb.ScopeSpans{{
				Spans: []*tracepb.Span{{TraceId: foreignTid[:]}},
			}},
		}},
	}
	_, err = l.Export(context.Background(), req2)
	if err != nil {
		t.Fatal(err)
	}
	if ring.slots[99&ring.mask].spanCount.Load() != 0 {
		t.Error("foreign span should not update ring")
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

func TestSweep(t *testing.T) {
	ring := newRing(10, 30)
	svcCount := 3
	timeoutNs := int64(30 * time.Second)
	old := time.Now().Add(-60 * time.Second).UnixNano()

	// Slot 0: error trace, incomplete (2 of 3 spans)
	ring.slots[0].generatedAt.Store(old)
	ring.slots[0].isError.Store(true)
	ring.slots[0].spanCount.Store(2)

	// Slot 1: non-error trace, unexpected ingestion
	ring.slots[1].generatedAt.Store(old)
	ring.slots[1].isError.Store(false)
	ring.slots[1].spanCount.Store(1)

	// Slot 2: non-error trace, correctly not ingested
	ring.slots[2].generatedAt.Store(old)
	ring.slots[2].isError.Store(false)
	ring.slots[2].spanCount.Store(0)

	// Slot 3: error trace, complete (3 of 3 spans) — not counted in incomplete
	ring.slots[3].generatedAt.Store(old)
	ring.slots[3].isError.Store(true)
	ring.slots[3].spanCount.Store(3)

	// Slot 4: too recent — must not be swept
	ring.slots[4].generatedAt.Store(time.Now().Add(-10 * time.Second).UnixNano())
	ring.slots[4].isError.Store(true)
	ring.slots[4].spanCount.Store(0)

	var sweepHead uint64
	incomplete, unexpected := sweep(ring, 5, time.Now(), timeoutNs, svcCount, &sweepHead)

	if incomplete != 1 {
		t.Errorf("incomplete = %d, want 1", incomplete)
	}
	if unexpected != 1 {
		t.Errorf("unexpected = %d, want 1", unexpected)
	}
	if sweepHead != 4 {
		t.Errorf("sweepHead = %d, want 4", sweepHead)
	}
	// Slots 0-3 must be zeroed
	for i := range 4 {
		if ring.slots[i].generatedAt.Load() != 0 {
			t.Errorf("slot %d generatedAt not zeroed after sweep", i)
		}
	}
	// Slot 4 must be untouched
	if ring.slots[4].generatedAt.Load() == 0 {
		t.Error("slot 4 should not be swept (too recent)")
	}
}
