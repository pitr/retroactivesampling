package buffer

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	hdrSize = 28 // traceID(16) + insertedAt(8) + dataLen(4)
)

var zeroID [16]byte

type deltaRecord struct {
	offset uint64
	size   uint32
}

type SpanBuffer struct {
	maxBytes      uint64
	f             *os.File
	wHead         uint64
	rHead         uint64
	used          uint64
	liveBytes     uint64
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
	return &SpanBuffer{
		maxBytes:      uint64(maxBytes),
		f:             f,
		entries:       make(map[[16]byte][]deltaRecord),
		evictObserver: evictObserver,
	}, nil
}

func (b *SpanBuffer) Close() error {
	b.mu.Lock()
	f := b.f
	if f == nil {
		b.mu.Unlock()
		return fmt.Errorf("already closed")
	}
	b.f = nil
	b.mu.Unlock()
	return f.Close()
}

func (b *SpanBuffer) WriteWithEviction(traceID [16]byte, data []byte, insertedAt time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	recSize := hdrSize + uint64(len(data))
	if recSize > b.maxBytes {
		return fmt.Errorf("record size %d exceeds ring capacity %d", recSize, b.maxBytes)
	}

	if b.wHead+recSize > b.maxBytes {
		remaining := b.maxBytes - b.wHead
		if remaining >= hdrSize {
			var hdr [hdrSize]byte
			binary.BigEndian.PutUint32(hdr[24:28], uint32(remaining-hdrSize))
			if _, err := b.f.WriteAt(hdr[:], int64(b.wHead)); err != nil {
				return err
			}
		}
		b.used += remaining
		b.wHead = 0
	}

	for b.used+recSize > b.maxBytes {
		if err := b.sweepOneLocked(insertedAt); err != nil {
			return err
		}
	}

	offset := b.wHead
	var hdr [hdrSize]byte
	copy(hdr[:16], traceID[:])
	binary.BigEndian.PutUint64(hdr[16:24], uint64(insertedAt.UnixNano()))
	binary.BigEndian.PutUint32(hdr[24:28], uint32(len(data)))
	if _, err := b.f.WriteAt(hdr[:], int64(offset)); err != nil {
		return err
	}
	if _, err := b.f.WriteAt(data, int64(offset+hdrSize)); err != nil {
		return err
	}

	b.entries[traceID] = append(b.entries[traceID], deltaRecord{offset, uint32(len(data))})
	b.wHead += recSize
	b.used += recSize
	b.liveBytes += recSize
	return nil
}

func (b *SpanBuffer) sweepOneLocked(now time.Time) error {
	if b.rHead >= b.maxBytes {
		b.rHead = 0
	}
	if b.rHead+hdrSize > b.maxBytes {
		b.used -= b.maxBytes - b.rHead
		b.rHead = 0
		return nil
	}

	var hdr [hdrSize]byte
	if _, err := b.f.ReadAt(hdr[:], int64(b.rHead)); err != nil {
		return err
	}
	dataLen := uint64(binary.BigEndian.Uint32(hdr[24:28]))
	recSize := uint64(hdrSize) + dataLen

	if bytes.Equal(hdr[:16], zeroID[:]) {
		b.used -= recSize
		b.rHead = 0
		return nil
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
	return nil
}

func (b *SpanBuffer) ReadAndDelete(traceID [16]byte) ([][]byte, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	deltas := b.entries[traceID]
	if len(deltas) == 0 {
		return nil, false, nil
	}

	result := make([][]byte, 0, len(deltas))
	for _, d := range deltas {
		chunk := make([]byte, d.size)
		if _, err := b.f.ReadAt(chunk, int64(d.offset+hdrSize)); err != nil {
			return nil, false, err
		}
		result = append(result, chunk)
		b.liveBytes -= uint64(hdrSize) + uint64(d.size)
	}
	delete(b.entries, traceID)
	return result, true, nil
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
