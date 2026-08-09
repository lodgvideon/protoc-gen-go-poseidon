package pgrpc

import (
	"context"
	"errors"
	"io"

	"github.com/lodgvideon/poseidon-http-client/conn"
	"github.com/lodgvideon/poseidon-http-client/grpc"
)

// StatusOf classifies any error a generated client can return, for metrics
// bucketing. It CLASSIFIES — it does not wrap, and the runtime never calls it.
//
// It exists because poseidon's error surface is not uniformly *Status. An RPC
// that reached the server and came back produces a *Status for every outcome,
// but a transport failure surfaces verbatim: grpc.ErrMessageTooLarge,
// grpc.ErrCompressed, grpc.ErrStreamClosed, grpc.ErrSendClosed,
// conn.ErrConnClosed, conn.ErrTooManyStreams, conn.ErrGoAway,
// conn.ErrConnDraining, or ctx.Err(). Code that assumes errors.As(err, &st)
// always matches drops every one of those into a zero-valued bucket — and a
// zero Status reads as OK, so a connection dying mid-run shows up in the
// numbers as success.
//
// The mapping:
//
//	*Status                             -> itself
//	context.DeadlineExceeded            -> DeadlineExceeded
//	context.Canceled                    -> Canceled
//	grpc.ErrMessageTooLarge             -> ResourceExhausted
//	grpc.ErrCompressed                  -> Internal (peer protocol violation)
//	conn.ErrConnClosed, ErrConnDraining,
//	  ErrGoAway, ErrTooManyStreams      -> Unavailable (retriable: reconnect)
//	grpc.ErrStreamClosed, ErrSendClosed -> Canceled (this client tore it down)
//	conn.ErrStreamClosed                -> Unavailable (peer reset)
//	io.EOF                              -> Internal (stream ended with no status)
//	anything else                       -> Unknown
//
// conn.ErrStreamClosed is in that table as a FALLBACK only. Every send-then-read
// site in this package tolerates it and falls through to the receive side,
// because it is the RFC 9113 §8.1 benign half-close on a call the server has
// already answered. If it reaches here, some call site forgot that tolerance.
func StatusOf(err error) Status {
	if err == nil {
		return Status{Code: OK}
	}

	// A real status outranks everything: it is the server's own diagnosis, and
	// the sentinel checks below would otherwise reclassify a wrapped one.
	var st *Status
	if errors.As(err, &st) {
		return *st
	}

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return Status{Code: DeadlineExceeded, Message: err.Error()}
	case errors.Is(err, context.Canceled):
		return Status{Code: Canceled, Message: err.Error()}

	case errors.Is(err, grpc.ErrMessageTooLarge):
		return Status{Code: ResourceExhausted, Message: err.Error()}
	case errors.Is(err, grpc.ErrCompressed):
		return Status{Code: Internal, Message: err.Error()}

	// The connection is gone or refusing new work. Retriable by reconnecting,
	// which is what Unavailable tells a caller to do.
	case errors.Is(err, conn.ErrConnClosed),
		errors.Is(err, conn.ErrConnDraining),
		errors.Is(err, conn.ErrGoAway),
		errors.Is(err, conn.ErrTooManyStreams):
		return Status{Code: Unavailable, Message: err.Error()}

	// This client tore the stream down. Canceled rather than Unavailable,
	// because retrying will not help: nothing failed on the wire.
	case errors.Is(err, grpc.ErrStreamClosed),
		errors.Is(err, grpc.ErrSendClosed):
		return Status{Code: Canceled, Message: err.Error()}

	// The peer reset the stream. Kept in its own case, apart from the two
	// above, because conn.ErrStreamClosed and grpc.ErrStreamClosed are distinct
	// sentinels with nearly identical names that mean opposite things: that one
	// is this client tearing the call down, this one is the other end doing it.
	// The order between them is not load-bearing — they are different values —
	// but merging the cases would be wrong, and a test pins them apart.
	case errors.Is(err, conn.ErrStreamClosed):
		return Status{Code: Unavailable, Message: err.Error()}

	case errors.Is(err, io.EOF):
		return Status{Code: Internal, Message: "stream ended without a grpc-status"}
	}

	return Status{Code: Unknown, Message: err.Error()}
}
