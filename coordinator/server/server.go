package server

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"sync"

	gen "retroactivesampling/proto"
)

type Server struct {
	gen.UnimplementedCoordinatorServer
	mu       sync.Mutex
	streams  map[string]gen.Coordinator_ConnectServer
	onNotify func(traceID string)
}

func New(onNotify func(traceID string)) *Server {
	return &Server{
		streams:  make(map[string]gen.Coordinator_ConnectServer),
		onNotify: onNotify,
	}
}

func (s *Server) Connect(stream gen.Coordinator_ConnectServer) error {
	id := randomID()
	s.mu.Lock()
	s.streams[id] = stream
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.streams, id)
		s.mu.Unlock()
	}()

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if n := msg.GetNotify(); n != nil {
			s.onNotify(n.TraceId)
		}
	}
}

// Broadcast sends keep decision to all connected processors. Best-effort.
func (s *Server) Broadcast(traceID string, keep bool) {
	msg := &gen.CoordinatorMessage{
		Payload: &gen.CoordinatorMessage_Decision{
			Decision: &gen.TraceDecision{TraceId: traceID, Keep: keep},
		},
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, stream := range s.streams {
		_ = stream.Send(msg)
	}
}

func randomID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return hex.EncodeToString(b)
}
