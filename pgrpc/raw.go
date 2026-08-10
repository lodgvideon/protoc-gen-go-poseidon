package pgrpc

import (
	"context"

	"github.com/lodgvideon/poseidon-http-client/conn"
	"github.com/lodgvideon/poseidon-http-client/grpc"
)

// THIS FILE IS THE SEAM. Every poseidon capability this package might adopt
// later is absorbed here, so that adopting it touches nothing else: not Unary's
// signature, not a caller's fields, not Client, not Invoker, and above all not
// one line of generated code.
//
// There are no capability probes. This module pins poseidon v0.12.0, where
// InvokeInto and RecvInto both exist; probing for a shipped API would be dead
// code that silently degrades if the assertion ever failed. The currently-open
// example of what WOULD land here is a SendFunc-style hook letting the codec
// marshal straight into poseidon's framing buffer, removing one full-message
// copy per Send. When that ships, the change is one line in this file.

// unaryRaw performs the wire half of a unary call.
//
// It delegates to poseidon's InvokeInto rather than reimplementing the call on
// NewStream, because InvokeInto already encodes three rules that are easy to
// get wrong and that this package would otherwise have to re-derive:
//
//  1. SendLast rather than Send followed by CloseSend, so END_STREAM rides the
//     message's own DATA frame — one fewer flush, TLS record and TCP segment
//     per RPC.
//  2. tolerating conn.ErrStreamClosed on the send. That is the RFC 9113 §8.1
//     case where the server answered without draining the request body, which
//     both net/http2 and grpc-go's server do for any handler that does not read
//     it. Every other send error stays fatal.
//  3. the mandatory SECOND Recv. Status and Trailer are populated only inside
//     poseidon's receive pump, so stopping after one Recv leaves Status() at
//     its zero value — reporting OK for a call that may have failed. The drain
//     also catches a server that sends two messages to a unary method.
//
// The cost is that InvokeInto discards response headers, trailers and the
// OK-path status. A caller who needs those has to open a stream instead.
//
// InvokeInto rather than Invoke: Invoke is literally InvokeInto with dst nil,
// so threading dst costs nothing and removes one allocation per RPC. On error
// it returns dst[:0] rather than nil, so a looping caller keeps its buffer —
// preserved here by returning whatever came back on both paths.
func unaryRaw(ctx context.Context, cc UnaryInvoker, method string, req, dst []byte,
	md []conn.HeaderField, pass []grpc.CallOption) ([]byte, error) {
	return cc.InvokeInto(ctx, method, req, dst, md, pass...)
}

// recvRaw is the streaming counterpart of unaryRaw, on the same seam.
//
// RecvInto rather than Recv: Recv is documented to return "a fresh copy owned
// by the caller", which is one allocation and one copy per message that no
// caller above it can decline. RecvInto appends into memory the stream already
// owns, so a server-streaming loop reads a million messages through one buffer.
func recvRaw(ctx context.Context, s *grpc.Stream, dst []byte) ([]byte, error) {
	return s.RecvInto(ctx, dst)
}
