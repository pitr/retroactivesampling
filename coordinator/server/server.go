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

type Server struct {
	gen.UnimplementedCoordinatorServer
	mu       sync.Mutex
	streams  map[string]gen.Coordinator_ConnectServer
	onNotify func(traceID string)
	bytesIn  Counter
	bytesOut Counter
}

func New(onNotify func(traceID string), bytesIn, bytesOut Counter) *Server {
	return &Server{
		streams:  make(map[string]gen.Coordinator_ConnectServer),
		onNotify: onNotify,
		bytesIn:  bytesIn,
		bytesOut: bytesOut,
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
		if s.bytesIn != nil {
			s.bytesIn.Add(float64(proto.Size(msg)))
		}
		if n := msg.GetNotify(); n != nil {
			s.onNotify(hex.EncodeToString(n.TraceId))
		}
	}
}

// Broadcast sends keep decision to all connected processors. Best-effort.
func (s *Server) Broadcast(traceID string) {
	tid, _ := hex.DecodeString(traceID)
	msg := &gen.CoordinatorMessage{
		Payload: &gen.CoordinatorMessage_Decision{
			Decision: &gen.TraceDecision{TraceId: tid},
		},
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := proto.Size(msg)
	for _, stream := range s.streams {
		_ = stream.Send(msg)
	}
	if s.bytesOut != nil {
		s.bytesOut.Add(float64(n * len(s.streams)))
	}
}

func randomID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return hex.EncodeToString(b)
}
