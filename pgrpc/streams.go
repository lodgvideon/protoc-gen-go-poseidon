package pgrpc

import (
	"context"
	"errors"
	"io"
	"iter"
	"sync/atomic"

	"github.com/lodgvideon/poseidon-http-client/grpc"
)

// terminal returns the latched outcome, or nil if the stream is still running.
// A stream that ended cleanly reports io.EOF, so that a caller looping on Recv
// sees the same end condition however it got there.
func (b *baseStream) terminal() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.ended {
		return nil
	}
	if b.termErr != nil {
		return b.termErr
	}
	return io.EOF
}

// cacheHeaderLocked records the response header block if it is not already
// cached. It runs on the receiving goroutine after a successful receive, which
// is the earliest point at which the headers are guaranteed to have arrived on
// a client-streaming or bidirectional stream.
func (b *baseStream) cacheHeader(ctx context.Context) {
	b.mu.Lock()
	done := b.hdrDone
	b.mu.Unlock()
	if done {
		return
	}
	// poseidon has already seen the block by now, so this does not block.
	if hdr, err := b.s.Header(ctx); err == nil {
		b.mu.Lock()
		b.hdr, b.hdrDone = hdr, true
		b.mu.Unlock()
	}
}

// sendSide is the state of a stream's send half. It is a separate type so that
// the client-streaming and bidirectional shapes, which are distinct generic
// types, share one implementation rather than two copies that drift.
type sendSide struct {
	sendBuf    []byte
	sendClosed bool
	// sendBusy makes a concurrent Send an attributable error rather than silent
	// wire corruption.
	//
	// It is an ATOMIC, not a lock, and it deliberately does not participate in
	// the Close gate. gate.RLock would not exclude anybody — RWMutex admits
	// unlimited readers — so two goroutines would write sendBuf and sendClosed
	// with no exclusion, making this wrapper LESS safe than the poseidon Stream
	// it wraps, which takes an exclusive mutex around its own send buffer. And
	// an exclusive gate lock is forbidden, because Close must be able to reach
	// the stream while a Send is parked on flow-control credit. A CAS is the
	// only shape that is both.
	sendBusy atomic.Bool
}

func (t *sendSide) enterSend() error {
	if !t.sendBusy.CompareAndSwap(false, true) {
		return ErrSendInFlight
	}
	return nil
}

func (t *sendSide) leaveSend() { t.sendBusy.Store(false) }

// doSend marshals m and writes it, half-closing the request side in the same
// DATA frame when last is set.
func doSend(ctx context.Context, b *baseStream, t *sendSide, m any, last bool) error {
	if err := t.enterSend(); err != nil {
		return err
	}
	defer t.leaveSend()

	if b.closed.Load() {
		return grpc.ErrStreamClosed
	}
	if t.sendClosed {
		return grpc.ErrSendClosed
	}

	wire, err := b.codec.MarshalAppend(t.sendBuf[:0], m)
	if err != nil {
		t.sendBuf = wire[:0] // keep the array; its contents are unspecified
		return NewCodecError(OpMarshal, b.codec.Name(), m, err)
	}
	t.sendBuf = wire // keep the grown array

	// poseidon copies wire into its own per-stream buffer BEFORE writing, so
	// sendBuf is reusable the moment this returns — on the error path too,
	// since the copy precedes the write.
	if last {
		if err := b.s.SendLast(ctx, wire); err != nil {
			return err
		}
		t.sendClosed = true
		return nil
	}
	return b.s.Send(ctx, wire)
}

// doCloseSend half-closes the request side. Idempotent.
func doCloseSend(ctx context.Context, b *baseStream, t *sendSide) error {
	if err := t.enterSend(); err != nil {
		return err
	}
	defer t.leaveSend()

	if b.closed.Load() {
		return grpc.ErrStreamClosed
	}
	if t.sendClosed {
		return nil
	}
	if err := b.s.CloseSend(ctx); err != nil {
		return err
	}
	t.sendClosed = true
	return nil
}

// ServerStream is one server-streaming call: one request, many responses.
type ServerStream[Resp any] struct{ baseStream }

// NewServerStream opens a server-streaming call and sends its single request.
//
// scratch may be nil, in which case the request buffer is allocated for this
// call. A reusable caller passes its own and gets reuse.
func NewServerStream[Resp any](ctx context.Context, cc StreamInvoker, cd Codec,
	cfg *CallConfig, method string, in any, scratch *[]byte) (*ServerStream[Resp], error) {
	s := &ServerStream[Resp]{}
	if err := initStream(&s.baseStream, ctx, cc, cd, cfg, method); err != nil {
		return nil, err
	}
	if scratch == nil {
		var b []byte
		scratch = &b
	}

	wire, err := cd.MarshalAppend((*scratch)[:0], in)
	if err != nil {
		*scratch = wire[:0]
		s.abort()
		return nil, NewCodecError(OpMarshal, cd.Name(), in, err)
	}
	*scratch = wire

	// SendLast rather than Send plus CloseSend: END_STREAM rides the message's
	// own DATA frame, saving a flush, a TLS record and usually a segment.
	if err := sendTolerant(s.s.SendLast(ctx, wire)); err != nil {
		s.abort()
		return nil, err
	}
	// Eager, and safe here specifically: the request is already complete, so a
	// server has everything it needs to start responding. The two other shapes
	// cannot do this — see ErrHeaderNotReady.
	if err := s.fetchHeader(ctx); err != nil {
		s.abort()
		return nil, err
	}
	return s, nil
}

// NewServerStreamOpts is the ergonomic form: it resolves options against the
// client's defaults and delegates.
func NewServerStreamOpts[Resp any](ctx context.Context, c *Client, method string,
	in any, opts ...CallOption) (*ServerStream[Resp], error) {
	var cfg CallConfig
	if err := c.resolve(&cfg, opts); err != nil {
		return nil, err
	}
	return NewServerStream[Resp](ctx, c.Invoker(), c.CodecFor(&cfg), &cfg, method, in, nil)
}

// Recv reads the next response into out. It returns io.EOF when the server
// completed the call successfully, and the call's error otherwise.
func (s *ServerStream[Resp]) Recv(ctx context.Context, out *Resp) error {
	s.gate.RLock()
	defer s.gate.RUnlock()
	if err := s.enterRecv(); err != nil {
		return err
	}
	defer s.leaveRecv()
	return s.recvLocked(ctx, out)
}

// recvLocked is the unguarded form, for callers that already hold the gate and
// the receive guard. All takes them once for a whole iteration, so the two must
// never nest.
func (s *ServerStream[Resp]) recvLocked(ctx context.Context, out *Resp) error {
	if err := s.terminal(); err != nil {
		return err
	}
	if err := s.recvOne(ctx, out); err != nil {
		s.finish(err)
		return err
	}
	s.cacheHeader(ctx)
	return nil
}

// RecvNew allocates a response and reads into it. Recv is the allocation-free
// form.
func (s *ServerStream[Resp]) RecvNew(ctx context.Context) (*Resp, error) {
	out := new(Resp)
	if err := s.Recv(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// All iterates the responses and CLOSES the stream when iteration finishes —
// on a break, a return, an error, or a panic.
//
// This is the documented default for server-streaming, and it is deadlock-free
// by construction: the receive and the Close both happen on the iterator's own
// goroutine, so the Close-must-not-race-a-Recv rule cannot be violated.
//
// io.EOF is not yielded. A successful end simply stops the iteration; inspect
// Status afterwards, which stays valid after Close.
func (s *ServerStream[Resp]) All(ctx context.Context) iter.Seq2[*Resp, error] {
	return func(yield func(*Resp, error) bool) {
		// Taken ONCE for the whole iteration rather than per message: a yield
		// body that also touched the stream would otherwise contend with the
		// loop itself.
		if err := s.enterRecv(); err != nil {
			yield(nil, err)
			return
		}
		func() {
			s.gate.RLock()
			defer s.gate.RUnlock()
			defer s.leaveRecv()
			for {
				out := new(Resp)
				err := s.recvLocked(ctx, out)
				if errors.Is(err, io.EOF) {
					return
				}
				if err != nil {
					yield(nil, err)
					return
				}
				if !yield(out, nil) {
					return
				}
			}
		}()
		// After the gate and the guard are released, or Close would park on
		// this goroutine's own read lock.
		_ = s.Close()
	}
}

// ClientStream is one client-streaming call: many requests, one response.
//
// It deliberately has NO Recv. poseidon permits concurrent Send and Recv — that
// is what makes bidirectional streaming work — but the receive side must be
// driven by one goroutine only. On this shape the natural mistake is a Recv
// loop in one goroutine while another sends, then CloseAndRecv from the sender:
// two goroutines inside poseidon's receive path, racing header, trailer,
// status, done, err and the decoder, none of which is guarded. Leaving Recv off
// the type makes that a compile error rather than a race.
type ClientStream[Req, Resp any] struct {
	baseStream
	sendSide
}

// NewClientStream opens a client-streaming call.
//
// initialSendCap pre-sizes the per-stream request buffer, so a caller that
// knows its message size can skip the first Send's growth ladder. Zero is fine.
func NewClientStream[Req, Resp any](ctx context.Context, cc StreamInvoker, cd Codec,
	cfg *CallConfig, method string, initialSendCap int) (*ClientStream[Req, Resp], error) {
	s := &ClientStream[Req, Resp]{}
	if err := initStream(&s.baseStream, ctx, cc, cd, cfg, method); err != nil {
		return nil, err
	}
	if initialSendCap > 0 {
		s.sendBuf = make([]byte, 0, initialSendCap)
	}
	return s, nil
}

// NewClientStreamOpts is the ergonomic form.
func NewClientStreamOpts[Req, Resp any](ctx context.Context, c *Client, method string,
	opts ...CallOption) (*ClientStream[Req, Resp], error) {
	var cfg CallConfig
	if err := c.resolve(&cfg, opts); err != nil {
		return nil, err
	}
	return NewClientStream[Req, Resp](ctx, c.Invoker(), c.CodecFor(&cfg), &cfg, method, 0)
}

// Send writes one request message.
func (s *ClientStream[Req, Resp]) Send(ctx context.Context, m *Req) error {
	return doSend(ctx, &s.baseStream, &s.sendSide, m, false)
}

// SendLastAndRecv writes the final request with END_STREAM on its own DATA
// frame and then reads the single response — one flush, one TLS record and
// usually one segment fewer than CloseAndRecv after a Send.
//
// A send failure does not always end the call: see the tolerance note inside.
func (s *ClientStream[Req, Resp]) SendLastAndRecv(ctx context.Context, m *Req, out *Resp) error {
	if err := doSend(ctx, &s.baseStream, &s.sendSide, m, true); err != nil {
		// Only the benign half-close falls through to the receive side. A
		// marshal failure or a real transport error is fatal, because nothing
		// is coming back and the receive would block until the deadline.
		//
		// Note also that poseidon LATCHES the first send failure, so once a
		// Send has failed, this one returns the latched error and never writes.
		if e := sendTolerant(err); e != nil {
			return e
		}
	}
	return s.recvSingle(ctx, out)
}

// CloseAndRecv half-closes the request side and reads the single response.
// Prefer SendLastAndRecv when the last message is known.
func (s *ClientStream[Req, Resp]) CloseAndRecv(ctx context.Context, out *Resp) error {
	if err := doCloseSend(ctx, &s.baseStream, &s.sendSide); err != nil {
		if e := sendTolerant(err); e != nil {
			return e
		}
	}
	return s.recvSingle(ctx, out)
}

// recvSingle reads the one response a client-streaming method returns, then
// drains to the terminal event.
//
// The drain is not optional. Status and the trailers are populated only inside
// poseidon's receive pump, so stopping after one message leaves the status at
// its zero value — reporting OK for a call that may have failed. It also
// catches a server that sends two messages to a single-response method.
func (s *ClientStream[Req, Resp]) recvSingle(ctx context.Context, out *Resp) (err error) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	if err := s.enterRecv(); err != nil {
		return err
	}
	defer s.leaveRecv()
	defer func() { s.finish(err) }()

	if e := s.terminal(); e != nil {
		return e
	}
	if err := s.recvOne(ctx, out); err != nil {
		if errors.Is(err, io.EOF) {
			// A bare io.EOF means the call completed with status OK but carried
			// no message, which a single-response method must not do.
			return &Status{Code: Internal, Message: "client-streaming method returned no message"}
		}
		return err
	}
	s.cacheHeader(ctx)

	// Recv, NOT RecvInto: handing the drain the response scratch would let a
	// second message overwrite the answer just unmarshalled.
	if _, err := s.s.Recv(ctx); !errors.Is(err, io.EOF) {
		if err == nil {
			return &Status{Code: Internal, Message: "client-streaming method returned more than one message"}
		}
		return err
	}
	return nil
}

// BidiStream is one bidirectional call. Its send and receive halves may be
// driven by two different goroutines; each half on its own must be driven by
// one.
type BidiStream[Req, Resp any] struct {
	baseStream
	sendSide
}

// NewBidiStream opens a bidirectional call. See NewClientStream for
// initialSendCap.
func NewBidiStream[Req, Resp any](ctx context.Context, cc StreamInvoker, cd Codec,
	cfg *CallConfig, method string, initialSendCap int) (*BidiStream[Req, Resp], error) {
	s := &BidiStream[Req, Resp]{}
	if err := initStream(&s.baseStream, ctx, cc, cd, cfg, method); err != nil {
		return nil, err
	}
	if initialSendCap > 0 {
		s.sendBuf = make([]byte, 0, initialSendCap)
	}
	return s, nil
}

// NewBidiStreamOpts is the ergonomic form.
func NewBidiStreamOpts[Req, Resp any](ctx context.Context, c *Client, method string,
	opts ...CallOption) (*BidiStream[Req, Resp], error) {
	var cfg CallConfig
	if err := c.resolve(&cfg, opts); err != nil {
		return nil, err
	}
	return NewBidiStream[Req, Resp](ctx, c.Invoker(), c.CodecFor(&cfg), &cfg, method, 0)
}

// Send writes one request message.
func (s *BidiStream[Req, Resp]) Send(ctx context.Context, m *Req) error {
	return doSend(ctx, &s.baseStream, &s.sendSide, m, false)
}

// SendLast writes the final request and half-closes in the same DATA frame.
func (s *BidiStream[Req, Resp]) SendLast(ctx context.Context, m *Req) error {
	return doSend(ctx, &s.baseStream, &s.sendSide, m, true)
}

// CloseSend half-closes the request side. Idempotent.
func (s *BidiStream[Req, Resp]) CloseSend(ctx context.Context) error {
	return doCloseSend(ctx, &s.baseStream, &s.sendSide)
}

// Recv reads the next response into out, returning io.EOF at a successful end.
func (s *BidiStream[Req, Resp]) Recv(ctx context.Context, out *Resp) error {
	s.gate.RLock()
	defer s.gate.RUnlock()
	if err := s.enterRecv(); err != nil {
		return err
	}
	defer s.leaveRecv()
	return s.recvLocked(ctx, out)
}

func (s *BidiStream[Req, Resp]) recvLocked(ctx context.Context, out *Resp) error {
	if err := s.terminal(); err != nil {
		return err
	}
	if err := s.recvOne(ctx, out); err != nil {
		s.finish(err)
		return err
	}
	s.cacheHeader(ctx)
	return nil
}

// All iterates the responses. Unlike the server-streaming form it does NOT
// close the stream: the sending goroutine's lifetime is not the iterator's, and
// a break silently cancelling another goroutine's in-flight Send would be
// surprising. Leak-proofing where one goroutine owns everything; explicit
// ownership where two do.
func (s *BidiStream[Req, Resp]) All(ctx context.Context) iter.Seq2[*Resp, error] {
	return func(yield func(*Resp, error) bool) {
		if err := s.enterRecv(); err != nil {
			yield(nil, err)
			return
		}
		s.gate.RLock()
		defer s.gate.RUnlock()
		defer s.leaveRecv()
		for {
			out := new(Resp)
			err := s.recvLocked(ctx, out)
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				yield(nil, err)
				return
			}
			if !yield(out, nil) {
				return
			}
		}
	}
}
