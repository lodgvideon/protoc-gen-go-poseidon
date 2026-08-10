package e2e

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/lodgvideon/protoc-gen-go-poseidon/testdata/helloworld"
	"github.com/lodgvideon/protoc-gen-go-poseidon/testdata/helloworld/poseidon"
)

// HOW TO READ THESE.
//
// The absolute allocs/op is NOT this client's cost. A grpc-go server runs in
// this process and its allocations land in the same counter — poseidon's own
// benchmark harness carries the identical caveat for the same reason.
//
// What is meaningful is the DIFFERENCE BETWEEN THE PAIRED ARMS. Each pair runs
// the same server, sends the same messages over the same transport, and differs
// only in how the client receives them. Every fixed cost — stream setup, the
// handler's own work, the server's per-message allocations — is identical on
// both sides and cancels. What is left is ours.
//
// This is the only honest way to measure the streaming path from inside one
// process: unlike the unary path, a stream cannot be driven through a fake,
// because poseidon exposes no constructor for grpc.Stream and its fields are
// unexported. There is no isolated number to report, so none is reported.

const benchMessages = 64

// benchServerStream is the shared body: open a server stream, drain it, close.
func benchServerStream(b *testing.B, drain func(ctx context.Context, s poseidon.Greeter_LotsOfRepliesClient) error) {
	b.Helper()
	addr := serve(b, &greeter{replies: benchMessages})
	cc := dial(b, addr)
	c := newClientOn(cc)
	ctx := context.Background()
	in := &helloworld.HelloRequest{Name: "n"}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		s, err := c.LotsOfReplies(ctx, in)
		if err != nil {
			b.Fatalf("open: %v", err)
		}
		if err := drain(ctx, s); err != nil {
			b.Fatalf("drain: %v", err)
		}
		if err := s.Close(); err != nil {
			b.Fatalf("close: %v", err)
		}
	}
}

// BenchmarkServerStream_RecvNew allocates a response message per message, which
// is what the ergonomic loop does.
func BenchmarkServerStream_RecvNew(b *testing.B) {
	benchServerStream(b, func(ctx context.Context, s poseidon.Greeter_LotsOfRepliesClient) error {
		for {
			_, err := s.RecvNew(ctx)
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
		}
	})
}

// BenchmarkServerStream_RecvInto reuses ONE response message for the whole
// stream. The difference from RecvNew is the client-side saving, because the
// server did exactly the same work in both arms.
func BenchmarkServerStream_RecvInto(b *testing.B) {
	out := &helloworld.HelloReply{}
	benchServerStream(b, func(ctx context.Context, s poseidon.Greeter_LotsOfRepliesClient) error {
		for {
			err := s.Recv(ctx, out)
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
		}
	})
}

// BenchmarkServerStream_All is the self-closing iterator, the documented
// default. It allocates a message per message like RecvNew, so its distance
// from RecvInto is the price of the ergonomics — worth knowing before choosing
// between them in a hot path.
func BenchmarkServerStream_All(b *testing.B) {
	addr := serve(b, &greeter{replies: benchMessages})
	cc := dial(b, addr)
	c := newClientOn(cc)
	ctx := context.Background()
	in := &helloworld.HelloRequest{Name: "n"}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		s, err := c.LotsOfReplies(ctx, in)
		if err != nil {
			b.Fatalf("open: %v", err)
		}
		for _, err := range s.All(ctx) {
			if err != nil {
				b.Fatalf("iterate: %v", err)
			}
		}
	}
}

// BenchmarkBidi_SendRecv pairs one send with one receive on a live
// bidirectional stream, reusing both messages. There is no second arm here: the
// point is that the number exists and does not grow per message, not a
// comparison.
func BenchmarkBidi_SendRecv(b *testing.B) {
	addr := serve(b, &greeter{})
	cc := dial(b, addr)
	c := newClientOn(cc)
	ctx := context.Background()

	s, err := c.BidiHello(ctx)
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()

	in := &helloworld.HelloRequest{Name: "n"}
	out := &helloworld.HelloReply{}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := s.Send(ctx, in); err != nil {
			b.Fatalf("send: %v", err)
		}
		if err := s.Recv(ctx, out); err != nil {
			b.Fatalf("recv: %v", err)
		}
	}
}
