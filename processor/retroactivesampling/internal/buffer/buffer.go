package buffer

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.opentelemetry.io/collector/pdata/ptrace"
)

const hdrSize = 44 // traceID(32) + insertedAt(8) + dataLen(4)

var zeroID [32]byte

const (
	cpMagic   = uint32(0x52494E47) // "RING"
	cpVersion = uint32(1)
)

type deltaRecord struct {
	offset int64
	size   int32
}

type SpanBuffer struct {
	dir      string
	maxBytes int64
	f        *os.File
	wHead    int64
	rHead    int64
	used     int64
	entries  map[string][]deltaRecord
	mu       sync.Mutex
}

func New(dir string, maxBytes int64) (*SpanBuffer, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("maxBytes must be positive, got %d", maxBytes)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	b := &SpanBuffer{dir: dir, maxBytes: maxBytes, entries: make(map[string][]deltaRecord)}
	if err := b.tryLoadCheckpoint(); err == nil {
		return b, nil
	}
	f, err := os.OpenFile(b.ringPath(), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return nil, err
	}
	if err := f.Truncate(maxBytes); err != nil {
		f.Close()
		return nil, err
	}
	b.f = f
	return b, nil
}

func (b *SpanBuffer) ringPath() string { return filepath.Join(b.dir, "buffer.ring") }
func (b *SpanBuffer) cpPath() string   { return filepath.Join(b.dir, "buffer.checkpoint") }

func (b *SpanBuffer) Close() error {
	b.mu.Lock()
	f := b.f
	b.f = nil
	err := b.saveCP()
	b.mu.Unlock()
	if f == nil {
		return fmt.Errorf("already closed")
	}
	if err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (b *SpanBuffer) WriteWithEviction(traceID string, spans ptrace.Traces, insertedAt time.Time) error {
	return fmt.Errorf("not implemented")
}

func (b *SpanBuffer) Read(traceID string) (ptrace.Traces, bool, error) {
	return ptrace.Traces{}, false, fmt.Errorf("not implemented")
}

func (b *SpanBuffer) Delete(traceID string) error {
	return fmt.Errorf("not implemented")
}

func (b *SpanBuffer) saveCP() error                { return nil }
func (b *SpanBuffer) tryLoadCheckpoint() error     { return fmt.Errorf("no checkpoint") }

// Suppress unused import warning — bytes and binary used in later tasks.
var _ = bytes.NewReader
var _ = binary.BigEndian
