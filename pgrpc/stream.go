package pgrpc

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"

	"github.com/lodgvideon/poseidon-http-client/conn"
	"github.com/lodgvideon/poseidon-http-client/grpc"
)

// baseStream holds what every streaming shape shares.
//
// Three synchronisation primitives, each with exactly one job. Collapsing them
// is the tempting mistake and each collapse has a specific failure:
//
//	gate     orders Close against in-flight RECEIVE operations
//	recvBusy admits one receiving goroutine
//	mu       guards the terminal and header cache against every other goroutine
//
// The gate cannot do recvBusy's job: RWMutex.RLock admits unlimited concurrent
// holders, so it excludes nobody from anybody. recvBusy cannot do the gate's
// job: Close must WAIT for a receive to finish, and an atomic cannot be waited
// on. And neither can do mu's job, because Status and Trailer are read from a
// goroutine that never receives at all, so nothing about the receive path
// orders those reads.
type baseStream struct {
	s     *grpc.Stream
	codec Codec

	// callCtx is derived from the constructor's ctx, and cancel is what makes
	// Close safe. poseidon's rule is "cancel that goroutine's context first,
	// then Close" — a wrapper owning no cancel func cannot implement it, and
	// would either race a blocked Recv or block forever.
	callCtx context.Context
	cancel  context.CancelFunc

	// gate orders Close against in-flight receives. Every receive holds RLock
	// for its duration; Close cancels and then takes Lock.
	//
	// THE SEND SIDE IS DELIBERATELY NOT GATED. conn.Stream.Close calls
	// wakeSendWaiters on the abandon path expressly so that a writer parked on
	// flow-control credit bails — Close-during-a-blocked-Send is a SUPPORTED
	// poseidon operation. A gate admitting the send side would turn it into a
	// three-goroutine deadlock: the parked Send holds the read lock, Close
	// parks on the write lock, and Go's RWMutex writer priority then parks the
	// receiving goroutine behind Close, so nothing drains DATA and no window is
	// ever granted. The send side uses an atomic instead.
	gate   sync.RWMutex
	closed atomic.Bool

	// recvBusy turns "two goroutines in a receive operation" — a silent data
	// race on poseidon's unguarded receive state — into an attributable error
	// rather than a panic: a load generator must not be brought down by one
	// virtual user's bug.
	recvBusy atomic.Bool

	// mu guards everything below it.
	mu sync.Mutex
	// ended and termErr latch the terminal outcome, first writer wins, so that
	// a caller who keeps calling Recv gets the same value rather than a fresh
	// allocation per call. A field declared and never written is how a status
	// comes to report OK for a failed call.
	ended   bool
	termErr error
	// termStatus and trailer are SNAPSHOTS taken when the stream ends, from the
	// receiving goroutine. Reading poseidon's Status() and Trailer() directly
	// from another goroutine would race the receive pump, and holding the gate
	// for read would not help — two RLock holders are not ordered against each
	// other.
	termStatus Status
	trailer    []conn.HeaderField
	// hdr caches the response header block, populated eagerly by the
	// constructor so that Header is a pure cache read.
	hdr     []conn.HeaderField
	hdrDone bool

	// recvBuf is the per-stream receive scratch threaded into RecvInto. Owned
	// by the receiving goroutine, which recvBusy guarantees there is only one
	// of.
	recvBuf []byte
}

// Context returns the stream's derived, cancellable context.
//
// Every operation still takes its OWN context, mirroring poseidon exactly: the
// context given to the constructor governs setup only — it is what produced the
// grpc-timeout header the SERVER sees — and poseidon does not retain it.
//
// Passing Context() to every operation is the normal thing to do and gives
// grpc-go-shaped call sites for one extra token. The API does not force it,
// because poseidon does not: a caller who genuinely wants sixty seconds of
// grpc-timeout on the server and two hundred milliseconds per client-side Send
// can express exactly that.
//
// Close unblocks an in-flight receive only if that receive used Context() or a
// child of it. If it did not, Close does not merely become unreliable — it
// BLOCKS on the gate until that private context expires or the receive returns.
func (b *baseStream) Context() context.Context { return b.callCtx }

// enterRecv claims the receive side. The caller must hold gate.RLock.
func (b *baseStream) enterRecv() error {
	if !b.recvBusy.CompareAndSwap(false, true) {
		return ErrRecvInFlight
	}
	return nil
}

// leaveRecv releases the receive side. Always deferred, so that a panicking
// codec does not poison the stream permanently.
func (b *baseStream) leaveRecv() { b.recvBusy.Store(false) }

// finish latches the terminal outcome and snapshots the peer's status and
// trailers. First writer wins. It must be called from the receiving goroutine.
func (b *baseStream) finish(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ended {
		return
	}
	b.ended = true
	b.termErr = err
	// Snapshotted here, while this goroutine is the only one touching the
	// receive state. After this the fields are pure cache reads.
	b.termStatus = b.s.Status()
	b.trailer = b.s.Trailer()
}

// Status returns the RPC's terminal status, and CANNOT report a false OK.
//
// poseidon populates its own status only from peer-DECLARED outcomes. A context
// cancellation, a closed or draining connection, a truncated message or an
// event-buffer overflow lands in poseidon's error field instead and leaves the
// status at its ZERO value — whose code is OK. Returning that verbatim would
// put every transport-level failure in a load generator's success bucket.
//
// So: a peer-declared failure wins; a clean end is OK; anything else is
// classified from the terminal error by StatusOf. A zero Status is returned
// only BEFORE the stream has ended, which Ended distinguishes.
func (b *baseStream) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.ended {
		return Status{}
	}
	if b.termStatus.Code != OK {
		return b.termStatus
	}
	if b.termErr == nil || errors.Is(b.termErr, io.EOF) {
		return Status{Code: OK}
	}
	return StatusOf(b.termErr)
}

// Trailer returns the trailing metadata, or nil before the stream has ended.
//
// Like Status it reads a snapshot taken when the stream ended, so unlike
// poseidon's own Trailer it is safe from any goroutine.
func (b *baseStream) Trailer() []conn.HeaderField {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.trailer
}

// Ended reports whether the stream has reached a terminal state. It is what
// tells "not finished yet" apart from "finished successfully", both of which
// Status reports as code OK.
func (b *baseStream) Ended() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ended
}

// Header returns the response metadata.
//
// On a SERVER-STREAMING stream it is populated eagerly by the constructor,
// which costs nothing on the ordinary path — poseidon's pump clones the header block unconditionally, so
// the fields are allocated whether or not anyone reads them — and buys two
// things. Header keeps working after Close, which poseidon's does not, and the
// documented default for server-streaming closes the stream for you. And it
// makes Header a pure cache read, so having it on a bidirectional stream is
// safe: a lazy Header would drive poseidon's unguarded receive pump, and
// calling Header from the SENDING goroutine is standard grpc-go practice — the
// default migration mistake, not an exotic one.
//
// On a CLIENT-STREAMING or BIDIRECTIONAL stream it CANNOT be eager, and the
// design that said otherwise deadlocks: a server sends its response headers
// when it starts responding, which on those shapes is only after the client has
// sent something — and a constructor blocking for them has not let the caller
// send anything yet. There it is populated by the first successful receive
// instead, and returns ErrHeaderNotReady before that. The cost of the honest
// version is that on those two shapes Header is only safe to call once a
// receive has landed.
//
// The context parameter is accepted for symmetry with poseidon and with the
// other operations, and is unused: this never blocks.
//
// TWO CAVEATS, both inherited from poseidon and neither removable:
//
//  1. The returned slice ALIASES the stream's header block. Treat it as
//     READ-ONLY: mutating a field's bytes changes what this stream reports and,
//     on the Trailers-Only shape, what Trailer reports too, because poseidon's
//     copy there is shallow. Read values with Get rather than indexing.
//  2. A nil error does NOT mean the call succeeded. On the Trailers-Only shape
//     one HEADERS frame with END_STREAM carries both the response headers and
//     grpc-status, and poseidon marks the headers seen before deriving the
//     terminal status — so this returns a block and no error for a call that
//     has already failed with, say, PERMISSION_DENIED. Classify with the error
//     from a receive operation, never with this one.
func (b *baseStream) Header(_ context.Context) ([]conn.HeaderField, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.hdrDone {
		return nil, ErrHeaderNotReady
	}
	return b.hdr, nil
}

// Close releases the stream. Idempotent, and CAS-guarded before the gate so a
// second Close does not park for no reason.
//
// RESIDUAL RULE, which is poseidon's own: Close cancels the derived context and
// then WAITS for any in-flight RECEIVE to return. A receive driven with a
// private, unrelated context is not woken by that cancellation, so Close blocks
// until that context expires or the receive completes. Drive receives with
// Context() or a child of it.
//
// It is safe to call from the receiving goroutine itself with no wait, which is
// what the self-closing iterator does and why that path is deadlock-free by
// construction.
func (b *baseStream) Close() error {
	if !b.closed.CompareAndSwap(false, true) {
		return nil
	}
	b.cancel()    // unblocks anything using Context() or a child
	b.gate.Lock() // waits for RECEIVE-side operations only
	defer b.gate.Unlock()

	// This is what wakes a writer parked on flow-control credit.
	err := b.s.Close()

	b.mu.Lock()
	if !b.ended {
		b.ended = true
		b.termErr = grpc.ErrStreamClosed
		b.termStatus = b.s.Status()
		b.trailer = b.s.Trailer()
	}
	b.mu.Unlock()
	return err
}

// initStream performs the part of construction every shape shares: option
// check, derived context, and the stream itself.
//
// On failure it leaves nothing behind. An abandoned stream holds a conn.Stream
// slot charged against MAX_CONCURRENT_STREAMS and leaves the server generating
// a response nobody reads, so a systematic failure here wedges the connection;
// an un-cancelled child context stays registered in its parent forever, and a
// per-run parent plus millions of failed calls is unbounded growth.
// It fills b in place rather than returning a baseStream, because baseStream
// contains two mutexes: returning one by value and embedding the copy is a
// copylocks error, and go vet's full check is enabled here.
func initStream(b *baseStream, ctx context.Context, cc StreamInvoker, cd Codec,
	cfg *CallConfig, method string) error {
	if err := cfg.Err(); err != nil {
		return err
	}
	callCtx, cancel := context.WithCancel(ctx)
	s, err := cc.NewStream(callCtx, method, cfg.Metadata(), cfg.PoseidonOptions()...)
	if err != nil {
		cancel()
		return err
	}
	b.s, b.codec, b.callCtx, b.cancel = s, cd, callCtx, cancel
	return nil
}

// abort tears a partially-constructed stream down. Every constructor error path
// goes through it.
func (b *baseStream) abort() {
	_ = b.s.Close()
	b.cancel()
}

// fetchHeader performs the eager header read the constructors rely on.
func (b *baseStream) fetchHeader(ctx context.Context) error {
	hdr, err := b.s.Header(ctx)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.hdr, b.hdrDone = hdr, true
	b.mu.Unlock()
	return nil
}

// sendTolerant performs a send whose failure may still leave a complete answer
// waiting on the receive side.
//
// RFC 9113 §8.1 lets a server that has written a complete response reset the
// stream with NO_ERROR rather than drain the request body, and both net/http2
// and grpc-go's server do exactly that for a handler that does not read it. The
// reset reaches this layer as conn.ErrStreamClosed ON A CALL THE SERVER HAS
// ALREADY ANSWERED — and Trailers-Only is, in poseidon's own words, how gRPC
// servers report most errors. Returning the send error there would discard a
// complete PERMISSION_DENIED sitting in the event buffer and report a transport
// failure instead.
//
// Every other send failure stays fatal, because nothing is coming back.
func sendTolerant(err error) error {
	if errors.Is(err, conn.ErrStreamClosed) {
		return nil
	}
	return err
}

// recvOne is the shared receive body: pull one message into the stream's
// scratch and unmarshal it into out. The caller holds the gate and the receive
// guard.
func (b *baseStream) recvOne(ctx context.Context, out any) error {
	wire, err := recvRaw(ctx, b.s, b.recvBuf[:0])
	// Kept on both paths: poseidon returns dst[:0] rather than nil on error so
	// a looping caller does not lose the array it has already grown.
	b.recvBuf = wire
	if err != nil {
		return err
	}
	if err := b.codec.Unmarshal(wire, out); err != nil {
		return NewCodecError(OpUnmarshal, b.codec.Name(), out, err)
	}
	return nil
}
