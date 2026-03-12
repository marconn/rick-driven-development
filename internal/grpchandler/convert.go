package grpchandler

import (
	"encoding/json"
	"time"

	"github.com/marconn/rick-event-driven-development/internal/event"
	pb "github.com/marconn/rick-event-driven-development/internal/grpchandler/proto"
)

// EnvelopeToProto converts an event.Envelope to its protobuf representation.
func EnvelopeToProto(env event.Envelope) *pb.EventEnvelope {
	return &pb.EventEnvelope{
		Id:            string(env.ID),
		Type:          string(env.Type),
		AggregateId:   env.AggregateID,
		Version:       int32(env.Version),
		SchemaVersion: int32(env.SchemaVersion),
		TimestampMs:   env.Timestamp.UnixMilli(),
		CausationId:   string(env.CausationID),
		CorrelationId: env.CorrelationID,
		Source:        env.Source,
		Payload:       []byte(env.Payload),
	}
}

// ProtoToEnvelope converts a protobuf EventEnvelope to event.Envelope.
// Defaults zero/negative timestamps to time.Now() — external handlers may
// return events without timestamps set.
func ProtoToEnvelope(pb *pb.EventEnvelope) event.Envelope {
	ts := time.UnixMilli(pb.TimestampMs)
	if pb.TimestampMs <= 0 {
		ts = time.Now()
	}
	return event.Envelope{
		ID:            event.ID(pb.Id),
		Type:          event.Type(pb.Type),
		AggregateID:   pb.AggregateId,
		Version:       int(pb.Version),
		SchemaVersion: int(pb.SchemaVersion),
		Timestamp:     ts,
		CausationID:   event.ID(pb.CausationId),
		CorrelationID: pb.CorrelationId,
		Source:        pb.Source,
		Payload:       json.RawMessage(pb.Payload),
	}
}
