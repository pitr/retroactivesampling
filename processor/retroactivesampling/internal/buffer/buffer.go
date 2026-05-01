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
	chunkSize    int
	decisionWait time.Duration
	onMatch      func(pcommon.TraceID, []byte)
	evictObs     func(time.Duration)

	stage  []byte
	stageN int

	scratch []byte

	mu     sync.Mutex
	cond   *sync.Cond
	wHead  int64
	rHead  int64
	used   int64
	closed bool

	interestEntries map[pcommon.TraceID]*list.Element
	interestList    list.List

	wg sync.WaitGroup
}

func New(
	file string,
	maxBytes int64,
	chunkSize int,
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
	if chunkSize < hdrSize*2 {
		return nil, fmt.Errorf("chunkSize must be >= %d (2*hdrSize), got %d", hdrSize*2, chunkSize)
	}
	maxBytes = (maxBytes / int64(chunkSize)) * int64(chunkSize)
	if maxBytes < 2*int64(chunkSize) {
		return nil, fmt.Errorf("maxBytes must be >= 2*chunkSize after rounding, got %d", maxBytes)
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
		chunkSize:       chunkSize,
		decisionWait:    decisionWait,
		onMatch:         onMatch,
		evictObs:        evictObs,
		stage:           make([]byte, chunkSize),
		scratch:         make([]byte, chunkSize),
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
		b.cond.Signal()
		return
	}
	el := b.interestList.PushFront(&interestEntry{id: traceID, addedAt: time.Now()})
	b.interestEntries[traceID] = el
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

// Write appends a span record. Returns ErrFull if no chunk slot is available.
// Returns an error if the record is larger than chunkSize-hdrSize.
func (b *SpanBuffer) Write(traceID pcommon.TraceID, data []byte, insertedAt time.Time) error {
	recSize := hdrSize + len(data)
	if recSize > b.chunkSize {
		return fmt.Errorf("record size %d exceeds chunk size %d", recSize, b.chunkSize)
	}
	if b.stageN+recSize > b.chunkSize {
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

func (b *SpanBuffer) flushStage() error {
	clear(b.stage[b.stageN:])
	b.mu.Lock()
	offset := b.wHead
	b.mu.Unlock()
	if _, err := b.f.WriteAt(b.stage, offset); err != nil {
		return err
	}
	b.mu.Lock()
	b.wHead = (offset + int64(b.chunkSize)) % b.maxBytes
	b.used += int64(b.chunkSize)
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
		if _, err := b.f.ReadAt(b.scratch, rHead); err != nil {
			b.mu.Lock()
			b.rHead = (b.rHead + int64(b.chunkSize)) % b.maxBytes
			b.used -= int64(b.chunkSize)
			b.cond.Broadcast()
			continue
		}

		pos := 0
		for pos+hdrSize <= b.chunkSize {
			var tid pcommon.TraceID
			copy(tid[:], b.scratch[pos:pos+16])
			if tid == (pcommon.TraceID{}) {
				break
			}
			nsec := int64(binary.BigEndian.Uint64(b.scratch[pos+16:]))
			dataLen := int(binary.BigEndian.Uint32(b.scratch[pos+24:]))
			recSize := hdrSize + dataLen
			if pos+recSize > b.chunkSize {
				break
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
				pos += recSize
				continue
			}
			if age >= b.decisionWait || pressure {
				b.evictObs(age)
				pos += recSize
				continue
			}
			remaining := b.decisionWait - age
			time.AfterFunc(remaining, func() {
				b.mu.Lock()
				b.cond.Signal()
				b.mu.Unlock()
			})
			fullyConsumed = false
			break
		}

		b.mu.Lock()
		if fullyConsumed {
			b.rHead = (rHead + int64(b.chunkSize)) % b.maxBytes
			b.used -= int64(b.chunkSize)
			b.pruneInterestLocked()
			b.cond.Broadcast()
		} else {
			b.cond.Wait() // wait for AfterFunc signal (or Close signal) before re-checking
		}
	}
}

// Flush writes any staged records to the ring buffer. Call after a batch of Write calls.
func (b *SpanBuffer) Flush() error {
	if b.stageN == 0 {
		return nil
	}
	return b.flushStage()
}

// Close flushes any staged records, signals the sweeper to drain, and closes the file.
func (b *SpanBuffer) Close() error {
	if b.stageN > 0 {
		_ = b.flushStage()
	}
	b.mu.Lock()
	b.closed = true
	b.cond.Signal()
	b.mu.Unlock()
	b.wg.Wait()
	return b.f.Close()
}
