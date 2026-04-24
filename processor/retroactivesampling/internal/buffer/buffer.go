package buffer

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"go.opentelemetry.io/collector/pdata/ptrace"
)

const (
	hdrSize = 28 // traceID(16) + insertedAt(8) + dataLen(4)
)

var (
	zeroID       [16]byte
	mmapPageSize = int64(os.Getpagesize())
)

type deltaRecord struct {
	offset int64
	size   int32
}

type SpanBuffer struct {
	maxBytes      int64
	f             *os.File
	data          []byte
	wHead         int64
	rHead         int64
	used          int64
	lastMadvise   int64
	entries       map[[16]byte][]deltaRecord
	evictObserver func(time.Duration)
	mu            sync.Mutex
}

func New(file string, maxBytes int64, evictObserver func(time.Duration)) (*SpanBuffer, error) {
	if evictObserver == nil {
		evictObserver = func(time.Duration) {}
	}
	if maxBytes < hdrSize*2 {
		return nil, fmt.Errorf("maxBytes must be at least %d, got %d", hdrSize*2, maxBytes)
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
	data, err := unix.Mmap(int(f.Fd()), 0, int(maxBytes), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	_ = unix.Madvise(data, unix.MADV_SEQUENTIAL)
	return &SpanBuffer{
		maxBytes:      maxBytes,
		f:             f,
		data:          data,
		entries:       make(map[[16]byte][]deltaRecord),
		evictObserver: evictObserver,
	}, nil
}

func (b *SpanBuffer) Close() error {
	b.mu.Lock()
	data := b.data
	if data == nil {
		b.mu.Unlock()
		return fmt.Errorf("already closed")
	}
	b.data = nil
	f := b.f
	b.f = nil
	b.mu.Unlock()
	if err := unix.Munmap(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func (b *SpanBuffer) WriteWithEviction(traceID [16]byte, spans ptrace.Traces, insertedAt time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	m := ptrace.ProtoMarshaler{}
	delta, err := m.MarshalTraces(spans)
	if err != nil {
		return err
	}

	recSize := int64(hdrSize + len(delta))
	if recSize > b.maxBytes {
		return fmt.Errorf("record size %d exceeds ring capacity %d", recSize, b.maxBytes)
	}

	if b.wHead+recSize > b.maxBytes {
		remaining := b.maxBytes - b.wHead
		if remaining >= hdrSize {
			var hdr [hdrSize]byte
			binary.BigEndian.PutUint32(hdr[24:28], uint32(remaining-hdrSize))
			copy(b.data[b.wHead:], hdr[:])
		}
		b.used += remaining
		b.wHead = 0
	}

	for b.used+recSize > b.maxBytes {
		b.sweepOneLocked(insertedAt)
	}

	offset := b.wHead
	copy(b.data[offset:], traceID[:])
	binary.BigEndian.PutUint64(b.data[offset+16:], uint64(insertedAt.UnixNano()))
	binary.BigEndian.PutUint32(b.data[offset+24:], uint32(len(delta)))
	copy(b.data[offset+hdrSize:], delta)

	b.entries[traceID] = append(b.entries[traceID], deltaRecord{offset, int32(len(delta))})
	b.wHead += recSize
	b.used += recSize
	return nil
}

func (b *SpanBuffer) sweepOneLocked(now time.Time) {
	if b.rHead >= b.maxBytes {
		b.rHead = 0
	}
	if b.rHead+hdrSize > b.maxBytes {
		b.used -= b.maxBytes - b.rHead
		b.rHead = 0
		b.madviseSwept()
		return
	}

	hdr := b.data[b.rHead : b.rHead+hdrSize]
	dataLen := int64(binary.BigEndian.Uint32(hdr[24:28]))
	recSize := int64(hdrSize) + dataLen

	if bytes.Equal(hdr[:16], zeroID[:]) {
		b.used -= recSize
		b.rHead = 0
		b.madviseSwept()
		return
	}

	var key [16]byte
	copy(key[:], hdr[:16])
	if deltas, ok := b.entries[key]; ok && len(deltas) > 0 && deltas[0].offset == b.rHead {
		insertedAt := time.Unix(0, int64(binary.BigEndian.Uint64(hdr[16:24])))
		b.evictObserver(now.Sub(insertedAt))
		b.entries[key] = deltas[1:]
		if len(b.entries[key]) == 0 {
			delete(b.entries, key)
		}
	}

	b.used -= recSize
	b.rHead += recSize
	if b.rHead >= b.maxBytes {
		b.rHead = 0
	}
	b.madviseSwept()
}

// madviseSwept releases swept pages from RSS. Only issues the syscall when
// rHead crosses a page boundary. On wrap (rHead==0), releases from
// lastMadvise to end of file.
func (b *SpanBuffer) madviseSwept() {
	if b.rHead == 0 {
		end := b.maxBytes &^ (mmapPageSize - 1)
		if end > b.lastMadvise {
			_ = unix.Madvise(b.data[b.lastMadvise:end], unix.MADV_DONTNEED)
		}
		b.lastMadvise = 0
		return
	}
	aligned := b.rHead &^ (mmapPageSize - 1)
	if aligned > b.lastMadvise {
		_ = unix.Madvise(b.data[b.lastMadvise:aligned], unix.MADV_DONTNEED)
		b.lastMadvise = aligned
	}
}

func (b *SpanBuffer) ReadAndDelete(traceID [16]byte) (ptrace.Traces, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	deltas := b.entries[traceID]
	if len(deltas) == 0 {
		return ptrace.Traces{}, false, nil
	}

	u := ptrace.ProtoUnmarshaler{}
	result := ptrace.NewTraces()
	for _, d := range deltas {
		buf := make([]byte, d.size)
		copy(buf, b.data[d.offset+hdrSize:])
		t, err := u.UnmarshalTraces(buf)
		if err != nil {
			return ptrace.Traces{}, false, err
		}
		t.ResourceSpans().MoveAndAppendTo(result.ResourceSpans())
	}
	delete(b.entries, traceID)
	return result, true, nil
}
