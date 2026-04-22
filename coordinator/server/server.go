package server

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"sync"

	"google.golang.org/protobuf/proto"

	gen "pitr.ca/retroactivesampling/proto"
)

type Counter interface{ Add(float64) }

type streamEntry struct {
	ch chan *gen.CoordinatorMessage
}

type Server struct {
	gen.UnimplementedCoordinatorServer
	mu       sync.Mutex
	streams  map[string]*streamEntry
	onNotify func(traceID string)
	bytesIn  Counter
	bytesOut Counter
}

func New(onNotify func(traceID string), bytesIn, bytesOut Counter) *Server {
	return &Server{
		streams:  make(map[string]*streamEntry),
		onNotify: onNotify,
		bytesIn:  bytesIn,
		bytesOut: bytesOut,
	}
}

func (s *Server) Connect(stream gen.Coordinator_ConnectServer) error {
	id := randomID()
	entry := &streamEntry{ch: make(chan *gen.CoordinatorMessage, 256)}

	s.mu.Lock()
	s.streams[id] = entry
	s.mu.Unlock()

	done := make(chan struct{})
	defer func() {
		close(done)
		s.mu.Lock()
		delete(s.streams, id)
		s.mu.Unlock()
	}()

	go func() {
		for {
			select {
			case msg := <-entry.ch:
				if s.bytesOut != nil {
					s.bytesOut.Add(float64(proto.Size(msg)))
				}
				_ = stream.Send(msg)
			case <-done:
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
			s.onNotify(hex.EncodeToString(n.TraceId))
		}
	}
}

// Broadcast sends keep decision to all connected processors. Best-effort: slow or
// backpressured streams are skipped (their channel is full).
func (s *Server) Broadcast(traceID string) {
	tid, _ := hex.DecodeString(traceID)
	msg := &gen.CoordinatorMessage{
		Payload: &gen.CoordinatorMessage_Decision{
			Decision: &gen.TraceDecision{TraceId: tid},
		},
	}
	s.mu.Lock()
	for _, entry := range s.streams {
		select {
		case entry.ch <- msg:
		default:
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
