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
		stage:           make([]byte, 0, 2*stageCap), // wrap padding can append up to stageCap extra bytes
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
//
// If the stage is non-empty, it is flushed before signalling the sweeper so
// that records currently in stage become visible to delivery. Without this
// flush, a freshly-written record could sit in stage indefinitely while the
// sweeper, scanning only the on-disk region, finds nothing to deliver.
func (b *SpanBuffer) AddInterest(traceID pcommon.TraceID) {
	b.mu.Lock()
	b.addInterestLocked(traceID)
	if len(b.stage) > 0 {
		// Compute wrap based on current state; ignore flush errors here — they
		// will surface on the next Write.
		wrap := b.wHead+int64(len(b.stage)) > b.maxBytes
		_ = b.flushLocked(wrap)
	}
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

// Write appends a record to the staging buffer. If the stage is full the
// record is flushed to disk first; if the disk ring is full the writer
// evicts records inline.
func (b *SpanBuffer) Write(traceID pcommon.TraceID, data []byte, insertedAt time.Time) error {
	recSize := hdrSize + int64(len(data))
	if recSize > b.maxBytes {
		return fmt.Errorf("record size %d exceeds ring capacity %d", recSize, b.maxBytes)
	}
	if recSize > int64(b.stageCap) {
		return fmt.Errorf("record size %d exceeds stage capacity %d", recSize, b.stageCap)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return fmt.Errorf("buffer closed")
	}

	// Flush if appending would cross the disk wrap boundary.
	if b.wHead+int64(len(b.stage))+recSize > b.maxBytes {
		if err := b.flushLocked(true); err != nil {
			return err
		}
	}

	// Flush if stage is full.
	if int64(len(b.stage))+recSize > int64(b.stageCap) {
		if err := b.flushLocked(false); err != nil {
			return err
		}
	}

	if b.closed {
		return fmt.Errorf("buffer closed")
	}

	// Append record to stage.
	stageOff := len(b.stage)
	b.stage = b.stage[:stageOff+int(recSize)]
	hdr := b.stage[stageOff : stageOff+hdrSize]
	copy(hdr[:16], traceID[:])
	binary.BigEndian.PutUint64(hdr[16:24], uint64(insertedAt.UnixNano()))
	binary.BigEndian.PutUint32(hdr[24:28], uint32(len(data)))
	copy(b.stage[stageOff+hdrSize:], data)
	b.used += recSize

	// If the just-written trace is already in the interest set (rare — production
	// callers gate Write with HasInterest), flush so the sweeper can deliver it
	// promptly.
	if b.hasInterestLocked(traceID) {
		_ = b.flushLocked(false)
	}

	return nil
}

// flushLocked drains the staging buffer to disk in a single Pwrite, evicting
// records inline if the ring lacks space. If wrap=true the stage is padded
// with a skip-record so that wHead lands exactly at maxBytes (then wraps to
// 0). Caller holds b.mu.
func (b *SpanBuffer) flushLocked(wrap bool) error {
	for b.flushing && !b.closed {
		b.flushDone.Wait()
	}
	if b.closed {
		return fmt.Errorf("buffer closed")
	}
	if len(b.stage) == 0 {
		if !wrap {
			return nil
		}
		// Wrap requested but no records to flush: write a skip-record header
		// directly at wHead so the sweeper skips the tail gap on read.
		gap := b.maxBytes - b.wHead
		if gap >= hdrSize {
			var pad [hdrSize]byte
			binary.BigEndian.PutUint32(pad[24:28], uint32(gap-hdrSize))
			b.flushing = true
			off := b.wHead
			b.mu.Unlock()
			_, err := unix.Pwrite(b.fd, pad[:], off)
			b.mu.Lock()
			b.flushing = false
			b.flushDone.Broadcast()
			if err != nil {
				return err
			}
		}
		b.used += gap
		b.wHead = 0
		select {
		case b.wakeC <- struct{}{}:
		default:
		}
		return nil
	}
	b.flushing = true
	defer func() {
		b.flushing = false
		b.flushDone.Broadcast()
	}()

	// Wrap padding: append a skip-record header so the on-disk write tiles
	// the remaining tail space exactly.
	if wrap {
		gap := b.maxBytes - b.wHead - int64(len(b.stage))
		if gap >= hdrSize {
			off := len(b.stage)
			padTotal := int(gap)
			b.stage = b.stage[:off+padTotal]
			pad := b.stage[off : off+hdrSize]
			for i := range pad[:16] {
				pad[i] = 0
			}
			binary.BigEndian.PutUint64(pad[16:24], 0)
			binary.BigEndian.PutUint32(pad[24:28], uint32(padTotal-hdrSize))
			// pad bytes after the header are not zeroed; sweeper skips them via dataLen.
			b.used += int64(padTotal)
		} else if gap > 0 {
			// Tail gap < hdrSize: leave it; sweeper's tail-gap rule advances rHead past it.
			b.used += gap
		}
	}

	flushLen := int64(len(b.stage))

	// Ensure disk has flushLen bytes free at wHead.
	if err := b.evictBatchedLocked(flushLen); err != nil {
		return err
	}

	// Pwrite with the lock released.
	off := b.wHead
	buf := b.stage // safe: only this writer owns stage during flush
	b.mu.Unlock()
	n, err := unix.Pwrite(b.fd, buf, off)
	b.mu.Lock()

	if err != nil {
		return err
	}
	if int64(n) != flushLen {
		return fmt.Errorf("short pwrite: got %d, want %d", n, flushLen)
	}

	b.wHead += flushLen
	if wrap {
		// After wrap-flush wHead == maxBytes; reset to 0.
		b.wHead = 0
	} else if b.wHead == b.maxBytes {
		b.wHead = 0
	}
	b.stage = b.stage[:0]
	// Kick sweeper so newly-flushed records become visible for delivery.
	select {
	case b.wakeC <- struct{}{}:
	default:
	}
	return nil
}

// evictBatchedLocked frees `need` bytes at the disk wHead by advancing rHead
// through uninteresting records. Interesting records are handed off to the
// sweeper for delivery; the writer waits on flushDone until the sweeper
// advances past them. Caller holds b.mu and owns the flush slot.
func (b *SpanBuffer) evictBatchedLocked(need int64) error {
	for {
		// Free disk space = maxBytes - (used - len(stage)).
		freeDisk := b.maxBytes - (b.used - int64(len(b.stage)))
		if freeDisk >= need {
			return nil
		}

		// Tail gap: fewer than hdrSize bytes before wrap. Skip to 0.
		if b.rHead+hdrSize > b.maxBytes {
			b.used -= b.maxBytes - b.rHead
			b.rHead = 0
			continue
		}

		// Batched read of headers starting at rHead. Performed under mu so
		// the scratch contents stay consistent with rHead — sweeper can't
		// advance rHead and rotate the ring around us mid-read.
		readLen := evictScanCap
		if remain := b.maxBytes - b.rHead; int64(readLen) > remain {
			readLen = int(remain)
		}
		buf := b.scratch[:readLen]
		n, err := b.f.ReadAt(buf, b.rHead)
		if err != nil && n == 0 {
			return err
		}
		buf = buf[:n]

		// Walk records inside the buffer.
		for pos := 0; pos+hdrSize <= len(buf); {
			hdr := buf[pos : pos+hdrSize]
			var tid pcommon.TraceID
			copy(tid[:], hdr[:16])
			dataLen := int64(binary.BigEndian.Uint32(hdr[24:28]))
			recSize := hdrSize + dataLen

			// Skip-record (zero traceID): drop it.
			if tid.IsEmpty() {
				b.used -= recSize
				b.rHead += recSize
				if b.rHead >= b.maxBytes {
					b.rHead = 0
				}
				pos += int(recSize)
				freeDisk = b.maxBytes - (b.used - int64(len(b.stage)))
				if freeDisk >= need {
					return nil
				}
				continue
			}

			// Stop if record extends beyond the read window — re-read on next iteration.
			if pos+int(recSize) > len(buf) {
				break
			}

			if b.hasInterestLocked(tid) {
				// Hand off to sweeper, wait for it to advance rHead past this record.
				targetRHead := b.rHead + recSize
				select {
				case b.wakeC <- struct{}{}:
				default:
				}
				for b.rHead < targetRHead && !b.closed {
					b.flushDone.Wait()
				}
				if b.closed {
					return fmt.Errorf("buffer closed")
				}
				// rHead advanced; re-evaluate from top of outer loop (geometry changed).
				break
			}

			insertedAt := time.Unix(0, int64(binary.BigEndian.Uint64(hdr[16:24])))
			b.evictObs(time.Since(insertedAt))
			b.used -= recSize
			b.rHead += recSize
			if b.rHead >= b.maxBytes {
				b.rHead = 0
				break // re-read at offset 0 next iteration
			}
			pos += int(recSize)

			freeDisk = b.maxBytes - (b.used - int64(len(b.stage)))
			if freeDisk >= need {
				return nil
			}
		}
	}
}

func (b *SpanBuffer) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return fmt.Errorf("already closed")
	}
	// Flush any pending stage so sweeper can deliver outstanding interesting records.
	if len(b.stage) > 0 {
		// Flush without wrap consideration; if wrap is needed it'll happen here
		// just like in Write. Compute the wrap flag once.
		wrap := b.wHead+int64(len(b.stage)) > b.maxBytes
		_ = b.flushLocked(wrap) // best-effort; ignore error to ensure Close progresses
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
		select {
		case <-b.wakeC:
		case <-b.closeC:
			return
		}

		// Drain pass: walk on-disk records, delivering interesting ones and
		// dropping uninteresting ones. Each record is claimed (rHead advanced
		// under mu) before we release mu for the user callback, so writer
		// eviction cannot advance past us mid-delivery.
		for {
			b.mu.Lock()
			if b.closed {
				b.mu.Unlock()
				return
			}
			diskUsed := b.used - int64(len(b.stage))
			if diskUsed <= 0 {
				b.mu.Unlock()
				break
			}
			rHead := b.rHead

			// Tail gap: fewer than hdrSize bytes before wrap. Skip to 0.
			if rHead+hdrSize > b.maxBytes {
				b.used -= b.maxBytes - rHead
				b.rHead = 0
				b.flushDone.Broadcast()
				b.mu.Unlock()
				continue
			}

			// Read header under mu.
			var hdr [hdrSize]byte
			if _, err := b.f.ReadAt(hdr[:], rHead); err != nil {
				b.mu.Unlock()
				return
			}
			var tid pcommon.TraceID
			copy(tid[:], hdr[:16])
			dataLen := int64(binary.BigEndian.Uint32(hdr[24:28]))
			recSize := hdrSize + dataLen

			// Skip-record (zero traceID).
			if tid.IsEmpty() {
				b.used -= recSize
				b.rHead += recSize
				if b.rHead >= b.maxBytes {
					b.rHead = 0
				}
				b.flushDone.Broadcast()
				b.mu.Unlock()
				continue
			}

			interesting := b.hasInterestLocked(tid)

			var pb *[]byte
			if interesting {
				pb = payloadPool.Get().(*[]byte)
				if int64(cap(*pb)) < dataLen {
					*pb = make([]byte, dataLen)
				} else {
					*pb = (*pb)[:dataLen]
				}
				// Read payload under mu. Writer cannot Pwrite to this offset
				// concurrently because mu is held.
				if _, err := b.f.ReadAt(*pb, rHead+hdrSize); err != nil {
					if cap(*pb) <= maxPoolPayload {
						payloadPool.Put(pb)
					}
					b.mu.Unlock()
					return
				}
			}

			// Claim the record: advance rHead and used under mu before
			// releasing for the callback.
			insertedAt := time.Unix(0, int64(binary.BigEndian.Uint64(hdr[16:24])))
			b.used -= recSize
			b.rHead += recSize
			if b.rHead >= b.maxBytes {
				b.rHead = 0
			}
			b.evictObs(time.Since(insertedAt))
			b.flushDone.Broadcast()
			b.mu.Unlock()

			if interesting {
				b.onMatch(tid, *pb)
				if cap(*pb) <= maxPoolPayload {
					payloadPool.Put(pb)
				}
			}
		}
	}
}
