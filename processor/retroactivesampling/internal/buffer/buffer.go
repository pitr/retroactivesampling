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
	if hdrSize+len(data) > pageSize {
		numChunks := (hdrSize + len(data) + pageSize - 1) / pageSize
		if int64(numChunks)*int64(pageSize) > b.maxBytes {
			return ErrFull
		}
	}
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

	stage := make([]byte, pageSize)
	stageN := 0
	wHead := int64(0)

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	flush := func() (closed bool, err error) {
		clear(stage[stageN:])
		for b.used.Load()+int64(pageSize) > b.maxBytes {
			select {
			case <-b.pageFreed:
			case <-b.closeCh:
				stageN = 0 // buffer full at shutdown; drop staged data
				return true, nil
			}
		}
		if _, err := b.f.WriteAt(stage, wHead); err != nil {
			return false, err
		}
		wHead = (wHead + int64(pageSize)) % b.maxBytes
		b.used.Add(int64(pageSize))
		stageN = 0
		select {
		case b.wakeup <- struct{}{}:
		default:
		}
		return false, nil
	}

	handle := func(req writeReq) {
		if hdrSize+len(req.data) <= pageSize {
			recSize := hdrSize + len(req.data)
			if stageN+recSize > pageSize {
				if closed, _ := flush(); closed {
					return
				}
			}
			copy(stage[stageN:], req.traceID[:])
			binary.BigEndian.PutUint64(stage[stageN+16:], uint64(req.insertedAt.UnixNano()))
			binary.BigEndian.PutUint32(stage[stageN+24:], uint32(len(req.data)))
			copy(stage[stageN+hdrSize:], req.data)
			stageN += recSize
			return
		}

		// Large record: must start at a chunk boundary.
		if stageN > 0 {
			if closed, _ := flush(); closed {
				return
			}
		}
		numChunks := (hdrSize + len(req.data) + pageSize - 1) / pageSize
		if int64(numChunks)*int64(pageSize) > b.maxBytes {
			return // unflushably large: drop silently
		}
		// Wait until all N pages can be claimed atomically.
		for b.used.Load()+int64(numChunks)*int64(pageSize) > b.maxBytes {
			select {
			case <-b.pageFreed:
			case <-b.closeCh:
				return
			}
		}
		// Write first chunk: header + up to (pageSize-hdrSize) data bytes.
		copy(stage[0:], req.traceID[:])
		binary.BigEndian.PutUint64(stage[16:], uint64(req.insertedAt.UnixNano()))
		binary.BigEndian.PutUint32(stage[24:], uint32(len(req.data)))
		n := copy(stage[hdrSize:], req.data)
		clear(stage[hdrSize+n:])
		// WriteAt failure here or in the continuation loop orphans the already-written chunks
		// (wHead advances but used is not incremented). Recovery would require zeroing orphaned
		// pages; omitted because WriteAt on a pre-Truncated local file is effectively infallible.
		if _, err := b.f.WriteAt(stage, wHead); err != nil {
			return
		}
		wHead = (wHead + int64(pageSize)) % b.maxBytes

		// Write continuation chunks.
		off := n
		for off < len(req.data) {
			n = copy(stage[0:], req.data[off:])
			clear(stage[n:])
			if _, err := b.f.WriteAt(stage, wHead); err != nil {
				return
			}
			wHead = (wHead + int64(pageSize)) % b.maxBytes
			off += n
		}
		// Only after ALL N chunks written: report occupancy and wake sweeper.
		b.used.Add(int64(numChunks) * int64(pageSize))
		select {
		case b.wakeup <- struct{}{}:
		default:
		}
	}

	for {
		select {
		case req := <-b.writeCh:
			handle(req)
		case <-ticker.C:
			if stageN > 0 {
				_, _ = flush()
			}
		case <-b.closeCh:
			// Drain remaining queued writes then flush any staged data.
			for {
				select {
				case req := <-b.writeCh:
					handle(req)
				default:
					if stageN > 0 {
						_, _ = flush()
					}
					return
				}
			}
		}
	}
}

func (b *SpanBuffer) processPage(scratch []byte, rHead int64, closing bool) (fullyConsumed bool, chunksConsumed int, minRemaining time.Duration) {
	used := b.used.Load()
	fullyConsumed = true
	chunksConsumed = 1
	needsWriteback := false
	pressure := used > b.maxBytes*3/4 || closing

	var pos int
	for pos+hdrSize <= pageSize {
		var tid pcommon.TraceID
		copy(tid[:], scratch[pos:pos+16])
		if tid == (pcommon.TraceID{}) {
			break
		}
		dataLen := int(binary.BigEndian.Uint32(scratch[pos+24:]))
		recSize := hdrSize + dataLen

		// Multi-chunk record: always starts at pos==0 (enforced by write path).
		if pos == 0 && recSize > pageSize {
			numChunks := (hdrSize + dataLen + pageSize - 1) / pageSize
			if tid == tombstoneID {
				chunksConsumed = numChunks
				break
			}
			if int64(numChunks)*int64(pageSize) > used {
				// Not all chunks flushed yet: wait.
				fullyConsumed = false
				break
			}
			nsec := int64(binary.BigEndian.Uint64(scratch[pos+16:]))
			insertedAt := time.Unix(0, nsec)

			b.interestMu.RLock()
			interesting := b.hasInterestRLocked(tid)
			b.interestMu.RUnlock()

			age := time.Since(insertedAt)
			if interesting || age >= b.decisionWait || pressure {
				payload := make([]byte, dataLen)
				copy(payload, scratch[hdrSize:pageSize])
				payloadOff := pageSize - hdrSize
				for i := 1; i < numChunks; i++ {
					chunkOff := (rHead + int64(i)*int64(pageSize)) % b.maxBytes
					remaining := min(dataLen-payloadOff, pageSize)
					_, _ = b.f.ReadAt(payload[payloadOff:payloadOff+remaining], chunkOff)
					payloadOff += remaining
				}
				if interesting {
					b.onMatch(tid, payload)
				} else {
					b.evictObs(age)
				}
				copy(scratch[0:], tombstoneID[:])
				needsWriteback = true
				chunksConsumed = numChunks
			} else {
				remaining := b.decisionWait - age
				if minRemaining == 0 || remaining < minRemaining {
					minRemaining = remaining
				}
				fullyConsumed = false
			}
			break // always exit pos loop after multi-chunk record
		}

		// Single-chunk record.
		if pos+recSize > pageSize {
			break
		}
		if tid == tombstoneID {
			pos += recSize
			continue
		}
		nsec := int64(binary.BigEndian.Uint64(scratch[pos+16:]))
		insertedAt := time.Unix(0, nsec)

		b.interestMu.RLock()
		interesting := b.hasInterestRLocked(tid)
		b.interestMu.RUnlock()

		age := time.Since(insertedAt)
		if interesting {
			payload := make([]byte, dataLen)
			copy(payload, scratch[pos+hdrSize:pos+recSize])
			b.onMatch(tid, payload)
			copy(scratch[pos:], tombstoneID[:])
			needsWriteback = true
			pos += recSize
			continue
		}
		if age >= b.decisionWait || pressure {
			b.evictObs(age)
			copy(scratch[pos:], tombstoneID[:])
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

	if needsWriteback {
		_, _ = b.f.WriteAt(scratch, rHead)
	}
	return fullyConsumed, chunksConsumed, minRemaining
}

func (b *SpanBuffer) runSweeper() {
	defer b.wg.Done()

	scratch := make([]byte, pageSize)
	rHead := int64(0)
	closing := false

	for {
		if b.used.Load() == 0 {
			if closing {
				return
			}
			select {
			case <-b.wakeup:
			case <-b.writerDone:
				closing = true
			}
			continue
		}

		if _, err := b.f.ReadAt(scratch, rHead); err != nil {
			// Unreadable page: skip it.
			rHead = (rHead + int64(pageSize)) % b.maxBytes
			b.used.Add(-int64(pageSize))
			select {
			case b.pageFreed <- struct{}{}:
			default:
			}
			continue
		}

		fullyConsumed, chunksConsumed, minRemaining := b.processPage(scratch, rHead, closing)

		if fullyConsumed {
			rHead = (rHead + int64(chunksConsumed)*int64(pageSize)) % b.maxBytes
			b.used.Add(-int64(chunksConsumed) * int64(pageSize))
			select {
			case b.pageFreed <- struct{}{}:
			default:
			}
		} else {
			if minRemaining > 0 {
				time.AfterFunc(minRemaining, func() {
					select {
					case b.wakeup <- struct{}{}:
					default:
					}
				})
			}
			select {
			case <-b.wakeup:
			case <-b.writerDone:
				closing = true
			}
		}
	}
}

// Close signals goroutines to stop and closes the file.
// Callers must stop calling Write before calling Close.
func (b *SpanBuffer) Close() error {
	close(b.closeCh)
	b.wg.Wait()
	return b.f.Close()
}

