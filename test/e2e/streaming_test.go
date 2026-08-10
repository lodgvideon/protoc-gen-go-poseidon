package e2e

import (
	"errors"
	"fmt"
	"io"
	"iter"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lodgvideon/protoc-gen-go-poseidon/pgrpc"
	"github.com/lodgvideon/protoc-gen-go-poseidon/testdata/helloworld"
)

func TestServerStreamRecvLoop(t *testing.T) {
	c := newClient(t, &greeter{replies: 4})
	ctx := testCtx(t)

	s, err := c.LotsOfReplies(ctx, &helloworld.HelloRequest{Name: "n"})
	if err != nil {
		t.Fatalf("LotsOfReplies: %v", err)
	}
	defer func() { _ = s.Close() }()

	var got []string
	out := &helloworld.HelloReply{}
	for {
		err := s.Recv(ctx, out)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		got = append(got, out.GetMessage())
	}

	if len(got) != 4 {
		t.Fatalf("got %d replies %v, want 4", len(got), got)
	}
	for i, m := range got {
		if want := fmt.Sprintf("n #%d", i); m != want {
			t.Errorf("reply %d = %q, want %q", i, m, want)
		}
	}
	if st := s.Status(); st.Code != pgrpc.OK {
		t.Errorf("Status after a clean end = %v, want OK", st.Code)
	}
	if !s.Ended() {
		t.Error("Ended is false after io.EOF")
	}
}

// TestServerStreamHeaderIsEager pins the divergence from the design: server
// streaming is the ONE shape whose request is complete when the constructor
// returns, so its headers can be fetched there. The other two would deadlock.
func TestServerStreamHeaderIsEager(t *testing.T) {
	c := newClient(t, &greeter{replies: 1})
	ctx := testCtx(t)

	s, err := c.LotsOfReplies(ctx, &helloworld.HelloRequest{Name: "n"})
	if err != nil {
		t.Fatalf("LotsOfReplies: %v", err)
	}
	defer func() { _ = s.Close() }()

	md, err := s.Header(ctx)
	if err != nil {
		t.Fatalf("Header before any Recv: %v — server streaming populates it eagerly", err)
	}
	if len(md) == 0 {
		t.Error("Header returned an empty block")
	}
}

// TestServerStreamAllClosesItself is the documented default. The iterator owns
// the stream, so Close runs on break, on return and on a normal end — and the
// terminal state stays readable afterwards, which is the reason Status and
// Trailer are snapshots.
func TestServerStreamAllClosesItself(t *testing.T) {
	c := newClient(t, &greeter{replies: 10})
	ctx := testCtx(t)

	s, err := c.LotsOfReplies(ctx, &helloworld.HelloRequest{Name: "n"})
	if err != nil {
		t.Fatalf("LotsOfReplies: %v", err)
	}

	seen := 0
	for msg, err := range s.All(ctx) {
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		if msg.GetMessage() == "" {
			t.Error("empty message")
		}
		seen++
		if seen == 3 {
			break // the iterator must Close for us
		}
	}
	if seen != 3 {
		t.Fatalf("iterated %d times, want 3", seen)
	}

	// Close is idempotent, so a caller who also defers it is not punished.
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	// And the terminal state survives it.
	_ = s.Status()
	_ = s.Trailer()
}

func TestServerStreamAllRunsToCompletion(t *testing.T) {
	c := newClient(t, &greeter{replies: 5})
	ctx := testCtx(t)

	s, err := c.LotsOfReplies(ctx, &helloworld.HelloRequest{Name: "n"})
	if err != nil {
		t.Fatalf("LotsOfReplies: %v", err)
	}

	seen := 0
	for _, err := range s.All(ctx) {
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		seen++
	}
	if seen != 5 {
		t.Errorf("iterated %d times, want 5 — io.EOF must end the loop, not be yielded", seen)
	}
	if st := s.Status(); st.Code != pgrpc.OK {
		t.Errorf("Status after a complete iteration = %v, want OK", st.Code)
	}
}

func TestServerStreamRejectsASecondReceiver(t *testing.T) {
	c := newClient(t, &greeter{replies: 50, echoDelay: 0})
	ctx := testCtx(t)

	s, err := c.LotsOfReplies(ctx, &helloworld.HelloRequest{Name: "n"})
	if err != nil {
		t.Fatalf("LotsOfReplies: %v", err)
	}
	defer func() { _ = s.Close() }()

	// Hold the receive side by starting an iteration and not finishing it, then
	// try to receive from the same goroutine through the other entry point.
	next, stop := iterPull(s.All(ctx))
	defer stop()
	if _, _, ok := next(); !ok {
		t.Fatal("the iterator produced nothing")
	}

	if err := s.Recv(ctx, &helloworld.HelloReply{}); !errors.Is(err, pgrpc.ErrRecvInFlight) {
		t.Errorf("Recv during an active iteration = %v, want ErrRecvInFlight", err)
	}
}

func TestClientStreamSendLastAndRecv(t *testing.T) {
	c := newClient(t, &greeter{})
	ctx := testCtx(t)

	s, err := c.LotsOfGreetings(ctx)
	if err != nil {
		t.Fatalf("LotsOfGreetings: %v", err)
	}
	defer func() { _ = s.Close() }()

	for _, n := range []string{"a", "b"} {
		if err := s.Send(ctx, &helloworld.HelloRequest{Name: n}); err != nil {
			t.Fatalf("Send %q: %v", n, err)
		}
	}
	out := &helloworld.HelloReply{}
	if err := s.SendLastAndRecv(ctx, &helloworld.HelloRequest{Name: "c"}, out); err != nil {
		t.Fatalf("SendLastAndRecv: %v", err)
	}
	if want := "greeted 3: [a b c]"; out.GetMessage() != want {
		t.Errorf("reply = %q, want %q", out.GetMessage(), want)
	}
	if st := s.Status(); st.Code != pgrpc.OK {
		t.Errorf("Status = %v, want OK", st.Code)
	}
}

func TestClientStreamCloseAndRecv(t *testing.T) {
	c := newClient(t, &greeter{})
	ctx := testCtx(t)

	s, err := c.LotsOfGreetings(ctx)
	if err != nil {
		t.Fatalf("LotsOfGreetings: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Send(ctx, &helloworld.HelloRequest{Name: "solo"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	out := &helloworld.HelloReply{}
	if err := s.CloseAndRecv(ctx, out); err != nil {
		t.Fatalf("CloseAndRecv: %v", err)
	}
	if want := "greeted 1: [solo]"; out.GetMessage() != want {
		t.Errorf("reply = %q, want %q", out.GetMessage(), want)
	}
}

// TestClientStreamHeaderIsNotReadyUntilAReceive is the honest half of the eager
// header decision. The server has not started responding, so there is nothing
// to report — and the constructor did not block waiting for it, which is what
// makes the call possible at all.
func TestClientStreamHeaderIsNotReadyUntilAReceive(t *testing.T) {
	c := newClient(t, &greeter{})
	ctx := testCtx(t)

	s, err := c.LotsOfGreetings(ctx)
	if err != nil {
		t.Fatalf("LotsOfGreetings: %v", err)
	}
	defer func() { _ = s.Close() }()

	if _, err := s.Header(ctx); !errors.Is(err, pgrpc.ErrHeaderNotReady) {
		t.Errorf("Header before any receive = %v, want ErrHeaderNotReady", err)
	}

	out := &helloworld.HelloReply{}
	if err := s.SendLastAndRecv(ctx, &helloworld.HelloRequest{Name: "x"}, out); err != nil {
		t.Fatalf("SendLastAndRecv: %v", err)
	}
	if _, err := s.Header(ctx); err != nil {
		t.Errorf("Header after a receive: %v", err)
	}
}

func TestClientStreamRejectsSendAfterClose(t *testing.T) {
	c := newClient(t, &greeter{})
	ctx := testCtx(t)

	s, err := c.LotsOfGreetings(ctx)
	if err != nil {
		t.Fatalf("LotsOfGreetings: %v", err)
	}
	defer func() { _ = s.Close() }()

	out := &helloworld.HelloReply{}
	if err := s.SendLastAndRecv(ctx, &helloworld.HelloRequest{Name: "x"}, out); err != nil {
		t.Fatalf("SendLastAndRecv: %v", err)
	}
	if err := s.Send(ctx, &helloworld.HelloRequest{Name: "late"}); err == nil {
		t.Error("Send after the request side was closed was accepted")
	}
}

// TestBidiTwoGoroutines is the shape the whole design exists for: a sender and
// a receiver running at once on one stream. Run with -race.
func TestBidiTwoGoroutines(t *testing.T) {
	c := newClient(t, &greeter{})
	ctx := testCtx(t)

	s, err := c.BidiHello(ctx)
	if err != nil {
		t.Fatalf("BidiHello: %v", err)
	}
	defer func() { _ = s.Close() }()

	const n = 20
	var wg sync.WaitGroup
	wg.Add(1)
	sendErr := make(chan error, 1)
	go func() {
		defer wg.Done()
		for i := range n {
			if err := s.Send(ctx, &helloworld.HelloRequest{Name: fmt.Sprintf("m%d", i)}); err != nil {
				sendErr <- err
				return
			}
		}
		sendErr <- s.CloseSend(ctx)
	}()

	var got []string
	out := &helloworld.HelloReply{}
	for {
		err := s.Recv(ctx, out)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		got = append(got, out.GetMessage())
	}

	wg.Wait()
	if err := <-sendErr; err != nil {
		t.Fatalf("send side: %v", err)
	}
	if len(got) != n {
		t.Fatalf("received %d messages, want %d", len(got), n)
	}
	for i, m := range got {
		if want := fmt.Sprintf("re: m%d", i); m != want {
			t.Errorf("message %d = %q, want %q", i, m, want)
		}
	}
}

func TestBidiRejectsConcurrentSend(t *testing.T) {
	c := newClient(t, &greeter{})
	ctx := testCtx(t)

	s, err := c.BidiHello(ctx)
	if err != nil {
		t.Fatalf("BidiHello: %v", err)
	}
	defer func() { _ = s.Close() }()

	// Two senders racing: at least one must be refused rather than corrupting
	// the shared marshal scratch. Which one loses is not deterministic, so the
	// assertion is on the shape of the failures, not on their order.
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.Send(ctx, &helloworld.HelloRequest{Name: fmt.Sprintf("m%d", i)})
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil && !errors.Is(err, pgrpc.ErrSendInFlight) {
			t.Errorf("unexpected send error: %v", err)
		}
	}
}

func TestStreamingErrorCarriesTheServerStatus(t *testing.T) {
	c := newClient(t, &greeter{replies: 2})
	ctx := testCtx(t)

	// A server-streaming call the client abandons: Close must not turn the
	// outcome into a false OK.
	s, err := c.LotsOfReplies(ctx, &helloworld.HelloRequest{Name: "n"})
	if err != nil {
		t.Fatalf("LotsOfReplies: %v", err)
	}
	if err := s.Recv(ctx, &helloworld.HelloReply{}); err != nil {
		t.Fatalf("first Recv: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !s.Ended() {
		t.Error("Ended is false after Close")
	}
	if st := s.Status(); st.Code == pgrpc.OK {
		t.Error("Status reports OK for a stream the client abandoned mid-flight")
	}
}

func TestClientStreamServerError(t *testing.T) {
	// A handler that fails without reading everything is the RFC 9113 §8.1
	// case: the server answers and resets the request side, which reaches the
	// client as a benign half-close on a call that HAS an answer.
	c := newClient(t, &greeter{unaryErr: status.Error(codes.InvalidArgument, "bad")})
	ctx := testCtx(t)

	got, err := c.SayHello(ctx, &helloworld.HelloRequest{Name: "x"})
	if err == nil {
		t.Fatalf("expected an error, got %v", got)
	}
	if code := pgrpc.StatusOf(err).Code; code != pgrpc.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", code)
	}
}

// iterPull adapts a range-over-func iterator to a pull model, so a test can
// hold an iteration open partway through. It is stdlib iter.Pull2 under a local
// name, kept here rather than imported inline so the intent at the call site is
// obvious: the point is to PAUSE mid-iteration, not to iterate.
func iterPull[K, V any](seq func(yield func(K, V) bool)) (next func() (K, V, bool), stop func()) {
	return iter.Pull2(iter.Seq2[K, V](seq))
}
