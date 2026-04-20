package buffer

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
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
	if f == nil {
		b.mu.Unlock()
		return fmt.Errorf("already closed")
	}
	b.f = nil
	err := b.saveCP()
	b.mu.Unlock()
	if err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (b *SpanBuffer) WriteWithEviction(traceID string, spans ptrace.Traces, insertedAt time.Time) error {
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
		if err := b.wrapLocked(); err != nil {
			return err
		}
	}

	for b.used+recSize > b.maxBytes {
		if err := b.sweepOneLocked(); err != nil {
			return err
		}
	}

	var hdr [hdrSize]byte
	copy(hdr[:32], traceID)
	binary.BigEndian.PutUint64(hdr[32:40], uint64(insertedAt.UnixNano()))
	binary.BigEndian.PutUint32(hdr[40:44], uint32(len(delta)))

	offset := b.wHead
	if _, err := b.f.WriteAt(hdr[:], offset); err != nil {
		return err
	}
	if len(delta) > 0 {
		if _, err := b.f.WriteAt(delta, offset+hdrSize); err != nil {
			return err
		}
	}

	b.entries[traceID] = append(b.entries[traceID], deltaRecord{offset, int32(len(delta))})
	b.wHead += recSize
	b.used += recSize
	return nil
}

func (b *SpanBuffer) wrapLocked() error {
	remaining := b.maxBytes - b.wHead
	if remaining >= hdrSize {
		var hdr [hdrSize]byte // all-zero traceID = skip record
		binary.BigEndian.PutUint32(hdr[40:44], uint32(remaining-hdrSize))
		if _, err := b.f.WriteAt(hdr[:], b.wHead); err != nil {
			return err
		}
	}
	b.used += remaining
	b.wHead = 0
	return nil
}

func (b *SpanBuffer) sweepOneLocked() error {
	if b.rHead >= b.maxBytes {
		b.rHead = 0
	}
	if b.rHead+hdrSize > b.maxBytes {
		remaining := b.maxBytes - b.rHead
		b.used -= remaining
		b.rHead = 0
		return nil
	}

	var hdr [hdrSize]byte
	if _, err := b.f.ReadAt(hdr[:], b.rHead); err != nil {
		return err
	}
	dataLen := int64(binary.BigEndian.Uint32(hdr[40:44]))
	recSize := int64(hdrSize) + dataLen

	if bytes.Equal(hdr[:32], zeroID[:]) {
		b.used -= recSize
		b.rHead = 0
		return nil
	}

	traceID := string(bytes.TrimRight(hdr[:32], "\x00"))
	if deltas, ok := b.entries[traceID]; ok && len(deltas) > 0 && deltas[0].offset == b.rHead {
		b.entries[traceID] = deltas[1:]
		if len(b.entries[traceID]) == 0 {
			delete(b.entries, traceID)
		}
	}

	b.used -= recSize
	b.rHead += recSize
	if b.rHead >= b.maxBytes {
		b.rHead = 0
	}
	return nil
}

func (b *SpanBuffer) Read(traceID string) (ptrace.Traces, bool, error) {
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
		if _, err := b.f.ReadAt(buf, d.offset+hdrSize); err != nil {
			return ptrace.Traces{}, false, err
		}
		t, err := u.UnmarshalTraces(buf)
		if err != nil {
			return ptrace.Traces{}, false, err
		}
		t.ResourceSpans().MoveAndAppendTo(result.ResourceSpans())
	}
	return result, true, nil
}

func (b *SpanBuffer) Delete(traceID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.entries, traceID)
	return nil
}

func (b *SpanBuffer) saveCP() error {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, cpMagic)
	binary.Write(&buf, binary.BigEndian, cpVersion)
	binary.Write(&buf, binary.BigEndian, b.maxBytes)
	binary.Write(&buf, binary.BigEndian, b.wHead)
	binary.Write(&buf, binary.BigEndian, b.rHead)
	binary.Write(&buf, binary.BigEndian, b.used)
	binary.Write(&buf, binary.BigEndian, uint32(len(b.entries)))
	for id, deltas := range b.entries {
		buf.WriteByte(byte(len(id)))
		buf.WriteString(id)
		binary.Write(&buf, binary.BigEndian, uint32(len(deltas)))
		for _, d := range deltas {
			binary.Write(&buf, binary.BigEndian, d.offset)
			binary.Write(&buf, binary.BigEndian, d.size)
		}
	}
	return os.WriteFile(b.cpPath(), buf.Bytes(), 0600)
}

func (b *SpanBuffer) tryLoadCheckpoint() error {
	data, err := os.ReadFile(b.cpPath())
	if err != nil {
		return err
	}
	r := bytes.NewReader(data)

	read := func(v any) error { return binary.Read(r, binary.BigEndian, v) }

	var mg, ver uint32
	if read(&mg) != nil || mg != cpMagic {
		return fmt.Errorf("bad magic")
	}
	if read(&ver) != nil || ver != cpVersion {
		return fmt.Errorf("bad version")
	}

	var maxBytes, wHead, rHead, used int64
	if err := read(&maxBytes); err != nil {
		return err
	}
	if maxBytes != b.maxBytes {
		return fmt.Errorf("maxBytes mismatch: checkpoint=%d configured=%d", maxBytes, b.maxBytes)
	}
	if err := read(&wHead); err != nil {
		return err
	}
	if err := read(&rHead); err != nil {
		return err
	}
	if err := read(&used); err != nil {
		return err
	}

	var entryCount uint32
	if err := read(&entryCount); err != nil {
		return err
	}

	entries := make(map[string][]deltaRecord, entryCount)
	for i := uint32(0); i < entryCount; i++ {
		var idLen uint8
		if err := read(&idLen); err != nil {
			return err
		}
		idBytes := make([]byte, idLen)
		if _, err := io.ReadFull(r, idBytes); err != nil {
			return err
		}
		var deltaCount uint32
		if err := read(&deltaCount); err != nil {
			return err
		}
		deltas := make([]deltaRecord, deltaCount)
		for j := uint32(0); j < deltaCount; j++ {
			if err := read(&deltas[j].offset); err != nil {
				return err
			}
			if err := read(&deltas[j].size); err != nil {
				return err
			}
		}
		entries[string(idBytes)] = deltas
	}

	f, err := os.OpenFile(b.ringPath(), os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	if info.Size() != b.maxBytes {
		f.Close()
		return fmt.Errorf("ring file size mismatch: got %d want %d", info.Size(), b.maxBytes)
	}

	b.f = f
	b.wHead, b.rHead, b.used = wHead, rHead, used
	b.entries = entries
	_ = os.Remove(b.cpPath())
	return nil
}
