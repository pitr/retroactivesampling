package buffer

import (
	"container/list"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"go.opentelemetry.io/collector/pdata/pcommon"
)

const (
	hdrSize        = 28      // traceID(16) + insertedAt(8) + dataLen(4)
	maxPoolPayload = 1 << 16 // 64 KiB; larger slices are not returned to the pool

	DefaultStageCap = 4096 // default in-memory staging buffer size
	evictScanCap    = 4096 // batched header read window in evictBatchedLocked
)

// payloadPool recycles payload buffers. onMatch receives a slice from this pool;
// it must not retain the slice beyond its return (the sweeper recycles it immediately).
var payloadPool = sync.Pool{New: func() any { b := make([]byte, 0, 512); return &b }}

type interestEntry struct {
	id      pcommon.TraceID
	addedAt time.Time
}

// SpanBuffer is a file-backed ring buffer for span data. A background sweeper
// goroutine reads records in insertion order; records for traces in the interest
// set are delivered via onMatch, others are discarded after decisionWait expires.
// The writer path never reads from disk.
//
// interestEntries + interestList form an inline TTL set of trace IDs ordered by
// insertion/refresh time (newest at front). Entries expire after decisionWait.
// Not goroutine-safe on their own; callers hold mu.
type SpanBuffer struct {
	maxBytes        int64
	f               *os.File
	wHead           int64
	rHead           int64
	used            int64 // disk-live bytes + len(stage)
	decisionWait    time.Duration
	interestEntries map[pcommon.TraceID]*list.Element
	interestList    list.List
	onMatch         func(pcommon.TraceID, []byte)
	evictObs        func(time.Duration)
	fd              int

	// Staging
	stage    []byte
	stageCap int
	scratch  []byte // batched header reads in evictBatchedLocked

	// Concurrency
	mu          sync.Mutex
	flushDone   *sync.Cond   // signals flush slot release / rHead advance
	flushing    bool         // a writer owns the flush slot
	wakeC       chan struct{} // writer/AddInterest → sweeper kick (buffered 1)
	closeC      chan struct{}
	sweeperDone chan struct{}
	closed      bool
}

func New(
	file string,
	maxBytes int64,
	decisionWait time.Duration,
	stageCap int,
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
	if maxBytes < hdrSize*2 {
		return nil, fmt.Errorf("maxBytes must be at least %d, got %d", hdrSize*2, maxBytes)
	}
	if stageCap < hdrSize*2 {
		return nil, fmt.Errorf("stageCap must be at least %d, got %d", hdrSize*2, stageCap)
	}
	// Only enforce the upper bound when the ring is large enough that both
	// constraints (stageCap >= hdrSize*2 and stageCap <= maxBytes/2) can be
	// simultaneously satisfied, i.e. maxBytes >= hdrSize*4.
	if maxBytes >= hdrSize*4 && int64(stageCap)*2 > maxBytes {
		return nil, fmt.Errorf("stageCap (%d) must be at most maxBytes/2 (%d)", stageCap, maxBytes/2)
	}
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(file, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if info.Size() < maxBytes {
		if err := f.Truncate(maxBytes); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	b := &SpanBuffer{
		maxBytes:        maxBytes,
		f:               f,
		fd:              int(f.Fd()),
		decisionWait:    decisionWait,
		interestEntries: make(map[pcommon.TraceID]*list.Element),
		onMatch:         onMatch,
		evictObs:        evictObs,
		stage:           make([]byte, 0, stageCap),
		stageCap:        stageCap,
		scratch:         make([]byte, evictScanCap),
		wakeC:           make(chan struct{}, 1),
		closeC:          make(chan struct{}),
		sweeperDone:     make(chan struct{}),
	}
	b.flushDone = sync.NewCond(&b.mu)
	go b.runSweeper()
	return b, nil
}

func (b *SpanBuffer) addInterestLocked(id pcommon.TraceID) {
	if el, ok := b.interestEntries[id]; ok {
		el.Value.(*interestEntry).addedAt = time.Now()
		b.interestList.MoveToFront(el)
		return
	}
	el := b.interestList.PushFront(&interestEntry{id: id, addedAt: time.Now()})
	b.interestEntries[id] = el
	for back := b.interestList.Back(); back != nil; back = b.interestList.Back() {
		entry := back.Value.(*interestEntry)
		if time.Since(entry.addedAt) < b.decisionWait {
			break
		}
		id := entry.id
		b.interestList.Remove(back)
		delete(b.interestEntries, id)
	}
}

func (b *SpanBuffer) hasInterestLocked(id pcommon.TraceID) bool {
	el, ok := b.interestEntries[id]
	if !ok {
		return false
	}
	if time.Since(el.Value.(*interestEntry).addedAt) >= b.decisionWait {
		b.interestList.Remove(el)
		delete(b.interestEntries, id)
		return false
	}
	return true
}

// AddInterest marks traceID for delivery; the sweeper will call onMatch when it
// encounters records for this trace instead of discarding them.
func (b *SpanBuffer) AddInterest(traceID pcommon.TraceID) {
	b.mu.Lock()
	b.addInterestLocked(traceID)
	b.mu.Unlock()
	select {
	case b.wakeC <- struct{}{}:
	default:
	}
}

// HasInterest reports whether traceID is in the interest set.
func (b *SpanBuffer) HasInterest(traceID pcommon.TraceID) bool {
	b.mu.Lock()
	ok := b.hasInterestLocked(traceID)
	b.mu.Unlock()
	return ok
}

// Write appends a record to the ring. If the ring is full it blocks until the
// sweeper frees space.
func (b *SpanBuffer) Write(traceID pcommon.TraceID, data []byte, insertedAt time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	recSize := hdrSize + int64(len(data))
	if recSize > b.maxBytes {
		return fmt.Errorf("record size %d exceeds ring capacity %d", recSize, b.maxBytes)
	}

	if b.wHead+recSize > b.maxBytes {
		remaining := b.maxBytes - b.wHead
		if remaining >= hdrSize {
			var pad [hdrSize]byte
			binary.BigEndian.PutUint32(pad[24:28], uint32(remaining-hdrSize))
			if _, err := b.f.WriteAt(pad[:], b.wHead); err != nil {
				return err
			}
		}
		b.used += remaining
		b.wHead = 0
	}

	for b.used+recSize > b.maxBytes {
		if b.closed {
			return fmt.Errorf("buffer closed")
		}
		select {
		case b.wakeC <- struct{}{}:
		default:
		}
		b.flushDone.Wait()
	}
	if b.closed {
		return fmt.Errorf("buffer closed")
	}

	var hdr [hdrSize]byte
	copy(hdr[:16], traceID[:])
	binary.BigEndian.PutUint64(hdr[16:24], uint64(insertedAt.UnixNano()))
	binary.BigEndian.PutUint32(hdr[24:28], uint32(len(data)))
	iovs := [2][]byte{hdr[:], data}
	if _, err := unix.Pwritev(b.fd, iovs[:], b.wHead); err != nil {
		return err
	}
	b.used += recSize
	b.wHead += recSize

	select {
	case b.wakeC <- struct{}{}:
	default:
	}
	return nil
}

func (b *SpanBuffer) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return fmt.Errorf("already closed")
	}
	b.closed = true
	b.flushDone.Broadcast()
	b.mu.Unlock()
	close(b.closeC)
	<-b.sweeperDone
	return b.f.Close()
}

func (b *SpanBuffer) runSweeper() {
	defer close(b.sweeperDone)
	for {
		// Wait until there is data to process.
		b.mu.Lock()
		for b.used == 0 && !b.closed {
			b.mu.Unlock()
			select {
			case <-b.wakeC:
			case <-b.closeC:
				return
			}
			b.mu.Lock()
		}
		if b.closed {
			b.mu.Unlock()
			return
		}
		rHead := b.rHead
		b.mu.Unlock()

		// Tail gap: fewer than hdrSize bytes remain before the wrap boundary —
		// no skip record was written because remaining < hdrSize. Skip to 0.
		if rHead+hdrSize > b.maxBytes {
			b.mu.Lock()
			b.used -= b.maxBytes - rHead
			b.rHead = 0
			b.mu.Unlock()
			b.flushDone.Broadcast()
			continue
		}

		// Read header without holding the lock. rHead is only advanced by this
		// goroutine, so the position is stable between the unlock above and the
		// lock below.
		var hdr [hdrSize]byte
		if _, err := b.f.ReadAt(hdr[:], rHead); err != nil {
			return
		}

		traceID := pcommon.TraceID(hdr[:16])
		dataLen := int64(binary.BigEndian.Uint32(hdr[24:28]))
		recSize := hdrSize + dataLen

		// Skip/pad record written at the wrap boundary (zero traceID).
		if traceID.IsEmpty() {
			b.mu.Lock()
			b.used -= recSize
			b.rHead = 0
			b.mu.Unlock()
			b.flushDone.Broadcast()
			continue
		}

		insertedAt := time.Unix(0, int64(binary.BigEndian.Uint64(hdr[16:24])))
		age := time.Since(insertedAt)

		// Check interest before the age gate: already-interesting records are
		// delivered immediately regardless of decisionWait.
		b.mu.Lock()
		interesting := b.hasInterestLocked(traceID)
		full := b.used+hdrSize >= b.maxBytes
		b.mu.Unlock()

		// Non-interesting records wait out decisionWait unless ring is full.
		if !interesting && age < b.decisionWait && !full {
			select {
			case <-time.After(b.decisionWait - age):
			case <-b.wakeC:
			case <-b.closeC:
				return
			}
			continue
		}

		b.mu.Lock()
		if !interesting {
			interesting = b.hasInterestLocked(traceID) // re-check: AddInterest may have fired during wait
		}
		b.used -= recSize
		b.rHead += recSize
		if b.rHead >= b.maxBytes {
			b.rHead = 0
		}
		b.mu.Unlock()
		b.flushDone.Broadcast()

		if interesting {
			pb := payloadPool.Get().(*[]byte)
			if int64(cap(*pb)) < dataLen {
				*pb = make([]byte, dataLen)
			} else {
				*pb = (*pb)[:dataLen]
			}
			if _, err := b.f.ReadAt(*pb, rHead+hdrSize); err == nil {
				b.onMatch(traceID, *pb)
			}
			if cap(*pb) <= maxPoolPayload {
				payloadPool.Put(pb)
			}
		}
		b.evictObs(age)
	}
}
