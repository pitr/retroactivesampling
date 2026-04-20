package buffer

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.opentelemetry.io/collector/pdata/ptrace"
)

type entry struct {
	insertedAt time.Time
	size       int64
}

type SpanBuffer struct {
	dir      string
	maxBytes int64

	mu         sync.Mutex
	entries    map[string]entry
	totalBytes int64
	onEvict    func(traceID string, insertedAt time.Time)
}

// New creates a SpanBuffer. onEvict, if non-nil, is called synchronously while
// b.mu is held on each eviction; it must not call any SpanBuffer method.
func New(dir string, maxBytes int64, onEvict func(string, time.Time)) (*SpanBuffer, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	b := &SpanBuffer{dir: dir, maxBytes: maxBytes, onEvict: onEvict, entries: make(map[string]entry)}
	return b, b.loadExisting()
}

func (b *SpanBuffer) Close() error { return nil }

func (b *SpanBuffer) tracePath(traceID string) string {
	return filepath.Join(b.dir, traceID+".bin")
}

func (b *SpanBuffer) loadExisting() error {
	fis, err := os.ReadDir(b.dir)
	if err != nil {
		return err
	}
	for _, fi := range fis {
		name := fi.Name()
		if fi.IsDir() || len(name) < 5 || name[len(name)-4:] != ".bin" {
			continue
		}
		traceID := name[:len(name)-4]
		data, err := os.ReadFile(filepath.Join(b.dir, name))
		if err != nil || len(data) < 8 {
			continue
		}
		nsec := int64(binary.BigEndian.Uint64(data[:8]))
		size := int64(len(data))
		b.entries[traceID] = entry{insertedAt: time.Unix(0, nsec), size: size}
		b.totalBytes += size
	}
	return nil
}

// writeLocked writes spans for traceID. First write records insertion time;
// subsequent writes merge spans (preserving original insertion time).
// Value layout on disk: [8 bytes insertion_ns][otlp proto bytes]
// Returns (true, nil) when a new trace key was created.
func (b *SpanBuffer) writeLocked(traceID string, spans ptrace.Traces, insertedAt time.Time) (bool, error) {
	m := ptrace.ProtoMarshaler{}
	path := b.tracePath(traceID)
	existing, isExisting := b.entries[traceID]

	var toWrite ptrace.Traces
	var ts time.Time

	if isExisting {
		raw, err := os.ReadFile(path)
		if err != nil {
			return false, err
		}
		u := ptrace.ProtoUnmarshaler{}
		prev, err := u.UnmarshalTraces(raw[8:])
		if err != nil {
			return false, err
		}
		spans.ResourceSpans().MoveAndAppendTo(prev.ResourceSpans())
		toWrite = prev
		ts = existing.insertedAt
	} else {
		toWrite = spans
		ts = insertedAt
	}

	data, err := m.MarshalTraces(toWrite)
	if err != nil {
		return false, err
	}

	var hdr [8]byte
	binary.BigEndian.PutUint64(hdr[:], uint64(ts.UnixNano()))
	fileData := append(hdr[:], data...)

	if err := os.WriteFile(path, fileData, 0600); err != nil {
		return false, err
	}

	newSize := int64(len(fileData))
	b.totalBytes -= existing.size
	b.totalBytes += newSize
	b.entries[traceID] = entry{insertedAt: ts, size: newSize}
	return !isExisting, nil
}

func (b *SpanBuffer) Read(traceID string) (ptrace.Traces, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := b.entries[traceID]; !ok {
		return ptrace.Traces{}, false, nil
	}
	data, err := os.ReadFile(b.tracePath(traceID))
	if err != nil {
		return ptrace.Traces{}, false, err
	}
	if len(data) < 8 {
		return ptrace.Traces{}, false, nil
	}
	u := ptrace.ProtoUnmarshaler{}
	t, err := u.UnmarshalTraces(data[8:])
	return t, err == nil, err
}

func (b *SpanBuffer) Delete(traceID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.deleteLocked(traceID)
}

func (b *SpanBuffer) deleteLocked(traceID string) error {
	e, ok := b.entries[traceID]
	if !ok {
		return nil
	}
	if err := os.Remove(b.tracePath(traceID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	delete(b.entries, traceID)
	b.totalBytes -= e.size
	return nil
}

// evictOldestLocked evicts the oldest trace except skipID. Caller must hold mu.
// Returns "" if nothing to evict or delete failed.
func (b *SpanBuffer) evictOldestLocked(skipID string) string {
	oldest, oldestTime := "", time.Time{}
	for id, e := range b.entries {
		if id == skipID {
			continue
		}
		if oldest == "" || e.insertedAt.Before(oldestTime) {
			oldest, oldestTime = id, e.insertedAt
		}
	}
	if oldest == "" {
		return ""
	}
	if b.deleteLocked(oldest) != nil {
		return ""
	}
	if b.onEvict != nil {
		b.onEvict(oldest, oldestTime)
	}
	return oldest
}

func (b *SpanBuffer) WriteWithEviction(traceID string, spans ptrace.Traces, insertedAt time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.maxBytes > 0 {
		m := ptrace.ProtoMarshaler{}
		est, _ := m.MarshalTraces(spans)
		incoming := int64(8 + len(est))
		existing := b.entries[traceID].size // 0 if new trace
		for b.totalBytes-existing+incoming > b.maxBytes {
			if b.evictOldestLocked(traceID) == "" {
				break
			}
		}
	}

	_, err := b.writeLocked(traceID, spans, insertedAt)
	return err
}
