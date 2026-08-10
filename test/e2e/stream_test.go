package e2e

import (
	"errors"
	"testing"
	"time"

	grpcgo "google.golang.org/grpc"

	"github.com/lodgvideon/protoc-gen-go-poseidon/pgrpc"
	"github.com/lodgvideon/protoc-gen-go-poseidon/pgrpc/protocodec"
	"github.com/lodgvideon/protoc-gen-go-poseidon/testdata/helloworld"
	"github.com/lodgvideon/protoc-gen-go-poseidon/testdata/helloworld/poseidon"
)

// maxStreams is deliberately tiny. A leaked stream is invisible on a connection
// that allows thousands; against four, the fifth abandoned iteration wedges the
// connection and the test fails with a timeout instead of passing quietly.
const maxStreams = 4

// A leak is also invisible against a server that FINISHES. If the handler sends
// its last reply and returns, the stream ends from the far side and poseidon
// releases the slot whether or not anyone called Close — so an abandoned
// iteration costs nothing and the test passes on a broken build. Mutation
// testing caught exactly that: restoring the defect left this suite green.
//
// So the fixture server keeps producing. Each abandoned stream then really does
// hold its slot, which is the condition the assertion needs.
var busyServer = &greeter{replies: 100_000, echoDelay: 2 * time.Millisecond}

func streamingClient(t *testing.T, impl *greeter, opts ...grpcgo.ServerOption) poseidon.GreeterClient {
	t.Helper()
	addr := serveWith(t, impl, opts...)
	return poseidon.NewGreeterClient(pgrpc.NewClient(dial(t, addr), pgrpc.WithCodec(protocodec.Codec{})))
}

// TestAllClosesOnPanic is the regression for an iterator whose Close was a
// TRAILING statement rather than a deferred one. A panic in the loop body
// unwound straight past it, so every panicking iteration kept its stream — and
// three documents promised the opposite.
//
// The assertion is not "Close was called"; it is that the connection still
// works after more abandoned iterations than it has stream slots.
func TestAllClosesOnPanic(t *testing.T) {
	c := streamingClient(t, busyServer, grpcgo.MaxConcurrentStreams(maxStreams))
	ctx := testCtx(t)
	req := &helloworld.HelloRequest{Name: "n"}

	for i := range maxStreams + 1 {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("iteration %d: the body did not panic", i)
				}
			}()
			s, err := c.LotsOfReplies(ctx, req)
			if err != nil {
				t.Fatalf("iteration %d: open: %v", i, err)
			}
			for range s.All(ctx) {
				panic("from the loop body")
			}
		}()
	}

	// If any of those streams leaked, this call cannot get a slot.
	done := make(chan error, 1)
	go func() {
		_, err := c.SayHello(ctx, req)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a call after %d abandoned iterations failed: %v", maxStreams+1, err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("the connection is wedged: %d abandoned iterations leaked their streams", maxStreams+1)
	}
}

// TestAllClosesOnBreak is the same property on the ordinary path. It passed
// before the fix too — break returns normally, so the trailing Close ran — and
// it is here so the panic case above is not the only thing holding the
// iterator's contract.
func TestAllClosesOnBreak(t *testing.T) {
	c := streamingClient(t, busyServer, grpcgo.MaxConcurrentStreams(maxStreams))
	ctx := testCtx(t)
	req := &helloworld.HelloRequest{Name: "n"}

	for i := range maxStreams + 1 {
		s, err := c.LotsOfReplies(ctx, req)
		if err != nil {
			t.Fatalf("iteration %d: open: %v", i, err)
		}
		for range s.All(ctx) {
			break
		}
	}

	if _, err := c.SayHello(ctx, req); err != nil {
		t.Fatalf("a call after %d broken iterations failed: %v", maxStreams+1, err)
	}
}

// TestCloseFromInsideAllDoesNotDeadlock is the regression for the gate being
// held across the loop body. Close waits for in-flight RECEIVES; an iterator
// that held the gate for the whole loop made Close wait for an iteration that
// could not finish until Close returned. Permanent, and it lost the goroutine
// and its stream slot — while baseStream.Close's own doc says a Close from the
// receiving goroutine is safe.
func TestCloseFromInsideAllDoesNotDeadlock(t *testing.T) {
	c := streamingClient(t, busyServer)
	ctx := testCtx(t)

	s, err := c.LotsOfReplies(ctx, &helloworld.HelloRequest{Name: "n"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range s.All(ctx) {
			_ = s.Close() // the call that used to park forever
			break
		}
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Close() from inside the loop body deadlocked")
	}
}

// TestRecvAfterCloseFromTheBody states what the caller sees afterwards: the
// stream is closed, not merely un-deadlocked.
func TestRecvAfterCloseFromTheBody(t *testing.T) {
	c := streamingClient(t, busyServer)
	ctx := testCtx(t)

	s, err := c.LotsOfReplies(ctx, &helloworld.HelloRequest{Name: "n"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for range s.All(ctx) {
		_ = s.Close()
		break
	}

	out := &helloworld.HelloReply{}
	if err := s.Recv(ctx, out); err == nil {
		t.Error("a receive after Close succeeded")
	} else if !errors.Is(err, pgrpc.StatusOf(err).Err()) && pgrpc.StatusOf(err).Code == pgrpc.OK {
		t.Errorf("a receive after Close classified as OK: %v", err)
	}
}
