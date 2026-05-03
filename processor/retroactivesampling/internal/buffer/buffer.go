package buffer

import (
	"container/list"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
)

const hdrSize = 28 // 16 traceID + 8 insertedAt(ns) + 4 dataLen

var ErrFull = errors.New("buffer full")

// tombstoneID marks a processed record in a page that still has pending records.
// First byte 0xFF is astronomically unlikely to collide with a real trace ID.
var tombstoneID = pcommon.TraceID{0xFF}

var pageSize = os.Getpagesize()

type interestEntry struct {
	id      pcommon.TraceID
	addedAt time.Time
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

	writeMu sync.Mutex // serializes Write/Flush/Close flush path
	stage   []byte
	stageN  int

	scratch []byte

	mu            sync.Mutex
	cond          *sync.Cond
	wHead         int64
	rHead         int64
	used          int64
	closed        bool
	wakeupPending bool // signal sent while sweeper was outside lock

	interestEntries map[pcommon.TraceID]*list.Element
	interestList    list.List

	wg sync.WaitGroup
}

func New(
	file string,
	maxBytes int64,
	decisionWait time.Duration,
	onMatch func(pcommon.TraceID, []byte),
	evictObs func(time.Duration),
) (*SpanBuffer, error) {
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
	b := &SpanBuffer{
		f:               f,
		maxBytes:        maxBytes,
		decisionWait:    decisionWait,
		onMatch:         onMatch,
		evictObs:        evictObs,
		stage:           make([]byte, pageSize),
		scratch:         make([]byte, pageSize),
		interestEntries: make(map[pcommon.TraceID]*list.Element),
	}
	b.cond = sync.NewCond(&b.mu)
	b.wg.Add(1)
	go b.runSweeper()
	return b, nil
}

// AddInterest marks traceID as interesting; buffered spans will be delivered via onMatch.
func (b *SpanBuffer) AddInterest(traceID pcommon.TraceID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if el, ok := b.interestEntries[traceID]; ok {
		b.interestList.MoveToFront(el)
		el.Value.(*interestEntry).addedAt = time.Now()
	} else {
		el := b.interestList.PushFront(&interestEntry{id: traceID, addedAt: time.Now()})
		b.interestEntries[traceID] = el
	}
	b.wakeupPending = true
	b.cond.Signal()
}

// HasInterest reports whether traceID is still within its decision window.
func (b *SpanBuffer) HasInterest(traceID pcommon.TraceID) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.hasInterestLocked(traceID)
}

func (b *SpanBuffer) hasInterestLocked(traceID pcommon.TraceID) bool {
	el, ok := b.interestEntries[traceID]
	if !ok {
		return false
	}
	if time.Since(el.Value.(*interestEntry).addedAt) >= b.decisionWait {
		b.interestList.Remove(el)
		delete(b.interestEntries, traceID)
		return false
	}
	return true
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

// Write appends a span record. Returns ErrFull if no page slot is available.
// Records larger than one page span multiple chunks; they start at a chunk boundary.
func (b *SpanBuffer) Write(traceID pcommon.TraceID, data []byte, insertedAt time.Time) error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()

	if hdrSize+len(data) <= pageSize {
		// Small record: accumulate in stage, flush when full.
		recSize := hdrSize + len(data)
		if b.stageN+recSize > pageSize {
			if err := b.flushStage(); err != nil {
				return err
			}
			b.mu.Lock()
			full := b.used == b.maxBytes
			b.mu.Unlock()
			if full {
				return ErrFull
			}
		}
		copy(b.stage[b.stageN:], traceID[:])
		binary.BigEndian.PutUint64(b.stage[b.stageN+16:], uint64(insertedAt.UnixNano()))
		binary.BigEndian.PutUint32(b.stage[b.stageN+24:], uint32(len(data)))
		copy(b.stage[b.stageN+28:], data)
		b.stageN += recSize
		return nil
	}

	// Large record: must start at a chunk boundary.
	if b.stageN > 0 {
		if err := b.flushStage(); err != nil {
			return err
		}
	}
	numChunks := (hdrSize + len(data) + pageSize - 1) / pageSize
	b.mu.Lock()
	available := b.maxBytes - b.used
	b.mu.Unlock()
	if int64(numChunks)*int64(pageSize) > available {
		return ErrFull
	}

	// First chunk: header + first (pageSize-hdrSize) bytes of data.
	copy(b.stage[0:], traceID[:])
	binary.BigEndian.PutUint64(b.stage[16:], uint64(insertedAt.UnixNano()))
	binary.BigEndian.PutUint32(b.stage[24:], uint32(len(data)))
	n := copy(b.stage[hdrSize:], data) // copies exactly pageSize-hdrSize bytes
	b.stageN = hdrSize + n
	if err := b.flushStage(); err != nil {
		return err
	}

	// Continuation chunks: raw data, no header.
	off := n
	for off < len(data) {
		n = copy(b.stage[0:], data[off:]) // copies min(pageSize, remaining)
		b.stageN = n                      // flushStage zeros stage[n:pageSize]
		if err := b.flushStage(); err != nil {
			return err
		}
		off += n
	}
	return nil
}

func (b *SpanBuffer) flushStage() error {
	clear(b.stage[b.stageN:])
	b.mu.Lock()
	offset := b.wHead
	b.mu.Unlock()
	if _, err := b.f.WriteAt(b.stage, offset); err != nil {
		return err
	}
	b.mu.Lock()
	b.wHead = (offset + int64(pageSize)) % b.maxBytes
	b.used += int64(pageSize)
	b.wakeupPending = true
	b.cond.Signal()
	b.mu.Unlock()
	b.stageN = 0
	return nil
}

func (b *SpanBuffer) runSweeper() {
	defer b.wg.Done()
	b.mu.Lock()
	for {
		for b.used == 0 && !b.closed {
			b.cond.Wait()
		}
		if b.closed && b.used == 0 {
			b.mu.Unlock()
			return
		}
		rHead := b.rHead
		b.mu.Unlock()

		fullyConsumed := true
		needsWriteback := false
		if _, err := b.f.ReadAt(b.scratch, rHead); err != nil {
			b.mu.Lock()
			b.rHead = (b.rHead + int64(pageSize)) % b.maxBytes
			b.used -= int64(pageSize)
			b.wakeupPending = true
			b.cond.Broadcast()
			continue
		}

		var minRemaining time.Duration
		pos := 0
		for pos+hdrSize <= pageSize {
			var tid pcommon.TraceID
			copy(tid[:], b.scratch[pos:pos+16])
			if tid == (pcommon.TraceID{}) {
				break
			}
			nsec := int64(binary.BigEndian.Uint64(b.scratch[pos+16:]))
			dataLen := int(binary.BigEndian.Uint32(b.scratch[pos+24:]))
			recSize := hdrSize + dataLen
			if pos+recSize > pageSize {
				break
			}
			if tid == tombstoneID {
				pos += recSize
				continue
			}
			insertedAt := time.Unix(0, nsec)

			b.mu.Lock()
			interesting := b.hasInterestLocked(tid)
			pressure := b.used > b.maxBytes*3/4 || b.closed
			b.mu.Unlock()

			age := time.Since(insertedAt)
			if interesting {
				payload := make([]byte, dataLen)
				copy(payload, b.scratch[pos+hdrSize:pos+recSize])
				b.onMatch(tid, payload)
				copy(b.scratch[pos:], tombstoneID[:])
				needsWriteback = true
				pos += recSize
				continue
			}
			if age >= b.decisionWait || pressure {
				b.evictObs(age)
				copy(b.scratch[pos:], tombstoneID[:])
				needsWriteback = true
				pos += recSize
				continue
			}
			remaining := b.decisionWait - age
			if minRemaining == 0 || remaining < minRemaining {
				minRemaining = remaining
			}
			fullyConsumed = false
			pos += recSize
		}

		if !fullyConsumed {
			if needsWriteback {
				_, _ = b.f.WriteAt(b.scratch, rHead)
			}
			time.AfterFunc(minRemaining, func() {
				b.mu.Lock()
				b.wakeupPending = true
				b.cond.Signal()
				b.mu.Unlock()
			})
		}

		b.mu.Lock()
		if fullyConsumed {
			b.rHead = (rHead + int64(pageSize)) % b.maxBytes
			b.used -= int64(pageSize)
			b.pruneInterestLocked()
			b.wakeupPending = true
			b.cond.Broadcast()
		} else {
			if !b.wakeupPending {
				b.cond.Wait()
			}
			b.wakeupPending = false
		}
	}
}

// Flush writes any staged records to the ring buffer. Call after a batch of Write calls.
func (b *SpanBuffer) Flush() error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if b.stageN == 0 {
		return nil
	}
	return b.flushStage()
}

// Close flushes any staged records, signals the sweeper to drain, and closes the file.
func (b *SpanBuffer) Close() error {
	b.writeMu.Lock()
	if b.stageN > 0 {
		_ = b.flushStage()
	}
	b.writeMu.Unlock()
	b.mu.Lock()
	b.closed = true
	b.wakeupPending = true
	b.cond.Signal()
	b.mu.Unlock()
	b.wg.Wait()
	return b.f.Close()
}
