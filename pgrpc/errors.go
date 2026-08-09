package pgrpc

import (
	"errors"
	"fmt"
	"sync/atomic"
)

// CodecOp names which half of a codec failed.
type CodecOp uint8

const (
	// OpMarshal is a failure encoding a request message.
	OpMarshal CodecOp = iota
	// OpUnmarshal is a failure decoding a response message.
	OpUnmarshal
)

// String returns "marshal" or "unmarshal".
func (o CodecOp) String() string {
	if o == OpUnmarshal {
		return "unmarshal"
	}
	return "marshal"
}

// CodecError reports a serialisation failure, carrying BOTH the gRPC status the
// failure maps to and the codec's own error.
//
// Unwrap returns two errors, so one value satisfies both idioms:
//
//	var st *pgrpc.Status
//	errors.As(err, &st)          // st.Code == pgrpc.Internal
//	errors.Is(err, someSentinel) // the codec's own cause
//
// The Internal mapping follows grpc-go, which returns Internal for marshal and
// unmarshal alike. grpc-go stringifies the cause and drops it; this type keeps
// it, because a load generator that cannot distinguish a malformed fixture from
// a malformed response cannot act on either.
type CodecError struct {
	// Op is the half that failed.
	Op CodecOp
	// Codec is the failing codec's Name.
	Codec string
	// Type is the message's Go type, formatted at construction so that this
	// error does not retain the message itself.
	Type string
	// Err is the codec's own error.
	Err error

	st Status
}

// NewCodecError builds a CodecError for message m, recording m's type without
// retaining m.
//
// It is exported because the zero value of CodecError would carry a zero Status
// — which reads as OK, the exact failure mode this type exists to prevent — so
// a custom Codec wrapper needs a way to construct one correctly. The runtime
// uses it for every marshal and unmarshal failure.
func NewCodecError(op CodecOp, codec string, m any, cause error) *CodecError {
	e := &CodecError{
		Op:    op,
		Codec: codec,
		Type:  fmt.Sprintf("%T", m),
		Err:   cause,
	}
	e.st = Status{Code: Internal, Message: e.Error()}
	return e
}

// Error implements error.
func (e *CodecError) Error() string {
	return fmt.Sprintf("pgrpc: codec %q failed to %s %s: %v", e.Codec, e.Op, e.Type, e.Err)
}

// Unwrap returns the mapped status first and the codec's own cause second, so
// that errors.As finds a *Status and errors.Is still finds the cause.
func (e *CodecError) Unwrap() []error {
	if e.Err == nil {
		return []error{&e.st}
	}
	return []error{&e.st, e.Err}
}

// Misuse sentinels. All three report a CALLER bug, and all three are errors
// rather than panics: a load generator must not be brought down by one virtual
// user's mistake.
var (
	// ErrCallerInUse reports a second concurrent RPC on one Caller. A Caller
	// owns a single marshal scratch and a single CallConfig, so it serves one
	// goroutine and one in-flight call.
	ErrCallerInUse = errors.New("pgrpc: caller already has an RPC in flight")

	// ErrSendInFlight reports a concurrent Send on one stream. The contract is
	// one sender goroutine, matching poseidon's; without the guard two senders
	// would corrupt the shared marshal scratch and the request body on the wire.
	ErrSendInFlight = errors.New("pgrpc: another Send is in flight on this stream")

	// ErrRecvInFlight reports a concurrent receive on one stream. poseidon's
	// receive state — header, trailer, status, done, err and the decoder — is
	// entirely unguarded and must be driven by one goroutine.
	ErrRecvInFlight = errors.New("pgrpc: another receive operation is in flight on this stream")
)

// Guard makes a second concurrent RPC on one Caller an attributable error
// rather than a corrupted request body. The zero value is ready to use.
//
// It lives here rather than in generated code so that a generated file imports
// exactly three packages — context, pgrpc, and its own message package — with
// no dependency on a stdlib concurrency primitive, and so that ErrCallerInUse
// is declared in exactly one place.
type Guard struct{ inUse atomic.Bool }

// Enter claims the guard, or reports ErrCallerInUse if it is already held.
func (g *Guard) Enter() error {
	if !g.inUse.CompareAndSwap(false, true) {
		return ErrCallerInUse
	}
	return nil
}

// Leave releases the guard. It is safe to call only after a successful Enter.
func (g *Guard) Leave() { g.inUse.Store(false) }
