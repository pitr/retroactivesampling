package buffer

import (
	"container/list"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
)

const hdrSize = 28 // 16 traceID + 8 insertedAt(ns) + 4 dataLen

var (
	ErrFull = errors.New("buffer full")

	// tombstoneID marks a processed record in a page that still has pending records.
	// First byte 0xFF is astronomically unlikely to collide with a real trace ID.
	tombstoneID = pcommon.TraceID{0xFF}
	pageSize    = os.Getpagesize()
)

type interestEntry struct {
	id      pcommon.TraceID
	addedAt time.Time
}

type writeReq struct {
	traceID    pcommon.TraceID
	data       []byte
	insertedAt time.Time
}

// SpanBuffer is a file-backed ring buffer for span data. A background sweeper
// goroutine reads records in insertion order; records for traces in the interest
// set are delivered via onMatch, others are discarded after decisionWait expires.
// Under high load (>3/4 full), sweeper discards without waiting.
type SpanBuffer struct {
	f            *os.File
	maxBytes     int64
	decisionWait time.Duration
	onMatch      func(pcommon.TraceID, []byte)
	evictObs     func(time.Duration)

	writeCh    chan writeReq
	wakeup     chan struct{}
	pageFreed  chan struct{}
	writerDone chan struct{}
	closeCh    chan struct{}
	used       atomic.Int64

	interestMu      sync.RWMutex
	interestEntries map[pcommon.TraceID]*list.Element
	interestList    list.List

	wg sync.WaitGroup
}

func New(file string, maxBytes int64, decisionWait time.Duration, onMatch func(pcommon.TraceID, []byte), evictObs func(time.Duration)) (*SpanBuffer, error) {
	if onMatch == nil {
		onMatch = func(pcommon.TraceID, []byte) {}
	}
	if evictObs == nil {
		evictObs = func(time.Duration) {}
	}
	if decisionWait <= 0 {
		return nil, fmt.Errorf("decisionWait must be positive, got %v", decisionWait)
	}
	maxBytes = (maxBytes / int64(pageSize)) * int64(pageSize)
	if maxBytes < 2*int64(pageSize) {
		return nil, fmt.Errorf("maxBytes must be >= 2*pageSize after rounding, got %d", maxBytes)
	}
	f, err := os.OpenFile(file, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Truncate(maxBytes); err != nil {
		_ = f.Close()
		return nil, err
	}
	pageCount := int(maxBytes / int64(pageSize))
	b := &SpanBuffer{
		f:               f,
		maxBytes:        maxBytes,
		decisionWait:    decisionWait,
		onMatch:         onMatch,
		evictObs:        evictObs,
		writeCh:         make(chan writeReq, pageCount),
		wakeup:          make(chan struct{}, 1),
		pageFreed:       make(chan struct{}, 1),
		writerDone:      make(chan struct{}),
		closeCh:         make(chan struct{}),
		interestEntries: make(map[pcommon.TraceID]*list.Element),
	}
	b.wg.Add(2)
	go b.runWriter()
	go b.runSweeper()
	return b, nil
}

// AddInterest marks traceID as interesting; buffered spans will be delivered via onMatch.
func (b *SpanBuffer) AddInterest(traceID pcommon.TraceID) {
	b.interestMu.Lock()
	b.pruneInterestLocked()
	if el, ok := b.interestEntries[traceID]; ok {
		b.interestList.MoveToFront(el)
		el.Value.(*interestEntry).addedAt = time.Now()
	} else {
		el := b.interestList.PushFront(&interestEntry{id: traceID, addedAt: time.Now()})
		b.interestEntries[traceID] = el
	}
	b.interestMu.Unlock()
	select {
	case b.wakeup <- struct{}{}:
	default:
	}
}

// HasInterest reports whether traceID is still within its decision window.
func (b *SpanBuffer) HasInterest(traceID pcommon.TraceID) bool {
	b.interestMu.RLock()
	defer b.interestMu.RUnlock()
	return b.hasInterestRLocked(traceID)
}

func (b *SpanBuffer) hasInterestRLocked(traceID pcommon.TraceID) bool {
	el, ok := b.interestEntries[traceID]
	if !ok {
		return false
	}
	return time.Since(el.Value.(*interestEntry).addedAt) < b.decisionWait
}

func (b *SpanBuffer) pruneInterestLocked() {
	for {
		back := b.interestList.Back()
		if back == nil {
			break
		}
		e := back.Value.(*interestEntry)
		if time.Since(e.addedAt) < b.decisionWait {
			break
		}
		b.interestList.Remove(back)
		delete(b.interestEntries, e.id)
	}
}

// Write enqueues a span record. Returns ErrFull if the write channel is full.
func (b *SpanBuffer) Write(traceID pcommon.TraceID, data []byte, insertedAt time.Time) error {
	select {
	case b.writeCh <- writeReq{traceID, data, insertedAt}:
		return nil
	default:
		return ErrFull
	}
}

func (b *SpanBuffer) runWriter() {
	defer b.wg.Done()
	defer close(b.writerDone)
}

func (b *SpanBuffer) runSweeper() {
	defer b.wg.Done()
	<-b.writerDone
}

// Close signals goroutines to stop and closes the file.
func (b *SpanBuffer) Close() error {
	close(b.closeCh)
	b.wg.Wait()
	return b.f.Close()
}

// Keep encoding/binary referenced to avoid import errors until sweeper is implemented.
var _ = binary.BigEndian
