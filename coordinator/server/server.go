package server

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"sync"

	"google.golang.org/protobuf/proto"

	gen "pitr.ca/retroactivesampling/proto"
)

const maxBatchSize = 256

type Counter interface{ Add(float64) }

type streamEntry struct {
	ch chan []byte
}

type Server struct {
	gen.UnimplementedCoordinatorServer
	mu           sync.Mutex
	streams      map[string]*streamEntry
	onNotify     func(traceID []byte)
	bytesIn      Counter
	bytesOut     Counter
	droppedSends Counter
	sendErrors   Counter
}

func New(onNotify func(traceID []byte), bytesIn, bytesOut, droppedSends, sendErrors Counter) *Server {
	return &Server{
		streams:      make(map[string]*streamEntry),
		onNotify:     onNotify,
		bytesIn:      bytesIn,
		bytesOut:     bytesOut,
		droppedSends: droppedSends,
		sendErrors:   sendErrors,
	}
}

func (s *Server) Connect(stream gen.Coordinator_ConnectServer) error {
	id := randomID()
	entry := &streamEntry{ch: make(chan []byte, 256)}

	s.mu.Lock()
	s.streams[id] = entry
	s.mu.Unlock()

	done := make(chan struct{})
	defer func() {
		// Remove from map first so Broadcast stops pushing to this entry,
		// then signal the sender goroutine. This bounds what's left in entry.ch.
		s.mu.Lock()
		delete(s.streams, id)
		s.mu.Unlock()
		close(done)
	}()

	go func() {
		for {
			select {
			case tid := <-entry.ch:
				ids := [][]byte{tid}
			drain:
				for len(ids) < maxBatchSize {
					select {
					case tid := <-entry.ch:
						ids = append(ids, tid)
					default:
						break drain
					}
				}
				msg := &gen.CoordinatorMessage{
					Payload: &gen.CoordinatorMessage_Batch{
						Batch: &gen.BatchTraceDecision{TraceIds: ids},
					},
				}
				if s.bytesOut != nil {
					s.bytesOut.Add(float64(proto.Size(msg)))
				}
				if err := stream.Send(msg); err != nil && s.sendErrors != nil {
					s.sendErrors.Add(1)
				}
			case <-done:
				if n := len(entry.ch); n > 0 && s.droppedSends != nil {
					s.droppedSends.Add(float64(n))
				}
				return
			}
		}
	}()

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if s.bytesIn != nil {
			s.bytesIn.Add(float64(proto.Size(msg)))
		}
		if n := msg.GetNotify(); n != nil {
			s.onNotify(n.TraceId)
		}
	}
}

// Broadcast sends a keep decision to all connected processors. Best-effort: slow or
// backpressured streams are skipped (their channel is full).
func (s *Server) Broadcast(traceID []byte) {
	s.mu.Lock()
	for _, entry := range s.streams {
		select {
		case entry.ch <- traceID:
		default:
			if s.droppedSends != nil {
				s.droppedSends.Add(1)
			}
		}
	}
	s.mu.Unlock()
}

func randomID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return hex.EncodeToString(b)
}
