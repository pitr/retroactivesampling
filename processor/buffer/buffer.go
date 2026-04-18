package buffer

import (
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

var (
	tracesBucket = []byte("traces")
	orderBucket  = []byte("order")
)

type SpanBuffer struct {
	db *bolt.DB
}

func New(path string) (*SpanBuffer, error) {
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(tracesBucket); err != nil {
			return err
		}
		_, err := tx.CreateBucketIfNotExists(orderBucket)
		return err
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &SpanBuffer{db: db}, nil
}

func (b *SpanBuffer) Close() error { return b.db.Close() }

// orderKey produces a big-endian timestamp prefix + traceID for FIFO cursor ordering.
func orderKey(ts time.Time, traceID string) []byte {
	key := make([]byte, 8+len(traceID))
	binary.BigEndian.PutUint64(key[:8], uint64(ts.UnixNano()))
	copy(key[8:], traceID)
	return key
}

// Write buffers spans for traceID. First write records insertion time in the order bucket.
// Subsequent writes for the same traceID append spans (preserving original insertion order).
// Value layout in traces bucket: [8 bytes insertion_ns][otlp proto bytes]
func (b *SpanBuffer) Write(traceID string, spans ptrace.Traces, insertedAt time.Time) error {
	m := ptrace.ProtoMarshaler{}
	data, err := m.MarshalTraces(spans)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	return b.db.Update(func(tx *bolt.Tx) error {
		tb := tx.Bucket(tracesBucket)
		ob := tx.Bucket(orderBucket)
		key := []byte(traceID)
		existing := tb.Get(key)

		if existing == nil {
			// New trace: record in order bucket with insertion timestamp as header
			if err := ob.Put(orderKey(insertedAt, traceID), nil); err != nil {
				return err
			}
			var hdr [8]byte
			binary.BigEndian.PutUint64(hdr[:], uint64(insertedAt.UnixNano()))
			return tb.Put(key, append(hdr[:], data...))
		}

		// Existing trace: keep original insertion timestamp, merge spans
		u := ptrace.ProtoUnmarshaler{}
		prev, err := u.UnmarshalTraces(existing[8:])
		if err != nil {
			return fmt.Errorf("unmarshal existing: %w", err)
		}
		spans.ResourceSpans().MoveAndAppendTo(prev.ResourceSpans())
		merged, err := m.MarshalTraces(prev)
		if err != nil {
			return fmt.Errorf("marshal merged: %w", err)
		}
		return tb.Put(key, append(existing[:8:8], merged...))
	})
}

// Read retrieves all buffered spans for traceID. ok=false means trace not found.
func (b *SpanBuffer) Read(traceID string) (ptrace.Traces, bool, error) {
	var data []byte
	err := b.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(tracesBucket).Get([]byte(traceID))
		if v != nil && len(v) >= 8 {
			data = make([]byte, len(v)-8)
			copy(data, v[8:])
		}
		return nil
	})
	if err != nil || data == nil {
		return ptrace.Traces{}, false, err
	}
	u := ptrace.ProtoUnmarshaler{}
	t, err := u.UnmarshalTraces(data)
	return t, err == nil, err
}

// Delete removes a trace from both buckets atomically.
func (b *SpanBuffer) Delete(traceID string) error {
	return b.db.Update(func(tx *bolt.Tx) error {
		tb := tx.Bucket(tracesBucket)
		key := []byte(traceID)
		v := tb.Get(key)
		if v == nil {
			return nil
		}
		tsNs := binary.BigEndian.Uint64(v[:8])
		if err := tx.Bucket(orderBucket).Delete(orderKey(time.Unix(0, int64(tsNs)), traceID)); err != nil {
			return err
		}
		return tb.Delete(key)
	})
}

// EvictOldest removes the oldest trace (by insertion time) to free space.
// Returns the evicted traceID, or "" if the buffer is empty.
func (b *SpanBuffer) EvictOldest() (string, error) {
	var evicted string
	err := b.db.Update(func(tx *bolt.Tx) error {
		ob := tx.Bucket(orderBucket)
		c := ob.Cursor()
		k, _ := c.First()
		if k == nil {
			return nil
		}
		traceID := string(k[8:])
		evicted = traceID
		if err := ob.Delete(k); err != nil {
			return err
		}
		return tx.Bucket(tracesBucket).Delete([]byte(traceID))
	})
	return evicted, err
}

// WriteWithEviction writes spans, evicting oldest traces if the disk is full.
func (b *SpanBuffer) WriteWithEviction(traceID string, spans ptrace.Traces, insertedAt time.Time) error {
	for {
		err := b.Write(traceID, spans, insertedAt)
		if err == nil {
			return nil
		}
		if !isDiskFull(err) {
			return err
		}
		id, evictErr := b.EvictOldest()
		if evictErr != nil {
			return evictErr
		}
		if id == "" {
			return err // disk full, nothing left to evict
		}
	}
}

func isDiskFull(err error) bool {
	return strings.Contains(err.Error(), "no space left")
}
