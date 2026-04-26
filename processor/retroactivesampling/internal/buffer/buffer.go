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
)

const (
	hdrSize = 28 // traceID(16) + insertedAt(8) + dataLen(4)
)

var (
	zeroID       [16]byte
	mmapPageSize = uint64(os.Getpagesize())
)

type deltaRecord struct {
	offset uint64
	size   uint32
}

type SpanBuffer struct {
	maxBytes      uint64
	f             *os.File
	data          []byte
	wHead         uint64
	rHead         uint64
	used          uint64
	liveBytes     uint64
	lastMadvise   uint64
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
		maxBytes:      uint64(maxBytes),
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

func (b *SpanBuffer) WriteWithEviction(traceID [16]byte, data []byte, insertedAt time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	recSize := uint64(hdrSize + len(data))
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
	binary.BigEndian.PutUint32(b.data[offset+24:], uint32(len(data)))
	copy(b.data[offset+hdrSize:], data)

	b.entries[traceID] = append(b.entries[traceID], deltaRecord{offset, uint32(len(data))})
	b.wHead += recSize
	b.used += recSize
	b.liveBytes += recSize
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
	dataLen := uint64(binary.BigEndian.Uint32(hdr[24:28]))
	recSize := uint64(hdrSize) + dataLen

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
		b.liveBytes -= recSize
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

func (b *SpanBuffer) ReadAndDelete(traceID [16]byte) ([][]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	deltas := b.entries[traceID]
	if len(deltas) == 0 {
		return nil, false
	}

	result := make([][]byte, 0, len(deltas))
	for _, d := range deltas {
		chunk := make([]byte, d.size)
		copy(chunk, b.data[d.offset+hdrSize:])
		result = append(result, chunk)
		b.liveBytes -= uint64(hdrSize) + uint64(d.size)
	}
	delete(b.entries, traceID)
	return result, true
}

func (b *SpanBuffer) LiveBytes() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return int64(b.liveBytes)
}

func (b *SpanBuffer) OrphanedBytes() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return int64(b.used - b.liveBytes)
}
