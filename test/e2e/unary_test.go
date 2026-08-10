package e2e

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/lodgvideon/protoc-gen-go-poseidon/pgrpc"
	"github.com/lodgvideon/protoc-gen-go-poseidon/pgrpc/protocodec"
	"github.com/lodgvideon/protoc-gen-go-poseidon/testdata/helloworld"
	"github.com/lodgvideon/protoc-gen-go-poseidon/testdata/helloworld/poseidon"
)

func TestUnary(t *testing.T) {
	c := newClient(t, &greeter{})

	got, err := c.SayHello(testCtx(t), &helloworld.HelloRequest{Name: "world"})
	if err != nil {
		t.Fatalf("SayHello: %v", err)
	}
	if got.GetMessage() != "hello world" {
		t.Errorf("reply = %q, want %q", got.GetMessage(), "hello world")
	}
}

// TestUnaryTrailersOnlyError is the shape poseidon's own comments call "how
// gRPC servers report most errors": a handler returning a status without ever
// writing a message, which grpc-go answers with a single HEADERS frame carrying
// END_STREAM. The status must survive that as a *pgrpc.Status with the server's
// own code — not as a transport error.
func TestUnaryTrailersOnlyError(t *testing.T) {
	c := newClient(t, &greeter{unaryErr: status.Error(codes.PermissionDenied, "nope")})

	_, err := c.SayHello(testCtx(t), &helloworld.HelloRequest{Name: "world"})
	if err == nil {
		t.Fatal("expected the server's error")
	}

	var st *pgrpc.Status
	if !errors.As(err, &st) {
		t.Fatalf("err = %T (%v), want a *pgrpc.Status", err, err)
	}
	if st.Code != pgrpc.PermissionDenied {
		t.Errorf("Code = %v, want PermissionDenied", st.Code)
	}
	if st.Message != "nope" {
		t.Errorf("Message = %q, want %q", st.Message, "nope")
	}
	if got := pgrpc.StatusOf(err).Code; got != pgrpc.PermissionDenied {
		t.Errorf("StatusOf = %v, want PermissionDenied", got)
	}
}

// TestUnaryErrorAfterHeaders covers the other error shape: the server sends
// response metadata first, so the failure arrives in TRAILERS after a normal
// header block rather than in a Trailers-Only response. poseidon derives the
// status from a different code path for each.
func TestUnaryErrorAfterHeaders(t *testing.T) {
	c := newClient(t, &greeter{
		unaryHeader: metadata.Pairs("x-server", "yes"),
		unaryErr:    status.Error(codes.ResourceExhausted, "slow down"),
	})

	_, err := c.SayHello(testCtx(t), &helloworld.HelloRequest{Name: "world"})
	var st *pgrpc.Status
	if !errors.As(err, &st) {
		t.Fatalf("err = %T (%v), want a *pgrpc.Status", err, err)
	}
	if st.Code != pgrpc.ResourceExhausted {
		t.Errorf("Code = %v, want ResourceExhausted", st.Code)
	}
}

// TestUnaryMetadataReachesTheServer asserts what the SERVER saw, not what the
// client believes it sent: the handler echoes x-tenant back into the reply.
func TestUnaryMetadataReachesTheServer(t *testing.T) {
	c := newClient(t, &greeter{})

	got, err := c.SayHello(testCtx(t), &helloworld.HelloRequest{Name: "world"},
		pgrpc.WithHeaderString("x-tenant", "acme"))
	if err != nil {
		t.Fatalf("SayHello with metadata: %v", err)
	}
	if want := "hello world [tenant=acme]"; got.GetMessage() != want {
		t.Errorf("reply = %q, want %q — the header did not reach the server", got.GetMessage(), want)
	}
}

// TestCallerUnaryOnTheWire drives the buffer-reusing face through many calls on
// one connection, reusing the same in and out messages.
//
// The point is correctness under reuse, not allocation counting: the out
// message is reset by the codec on every call, and a merge-shaped unmarshal or
// a stale scratch would show up here as a wrong or accumulated reply. The
// allocation property itself is measured in pgrpc's own tests, where there is
// no in-process server inflating the count.
func TestCallerUnaryOnTheWire(t *testing.T) {
	x := newCaller(t, &greeter{})
	ctx := testCtx(t)

	in := &helloworld.HelloRequest{}
	out := &helloworld.HelloReply{}
	for i := range 25 {
		in.Name = fmt.Sprintf("n%d", i)
		if err := x.SayHello(ctx, in, out); err != nil {
			t.Fatalf("SayHello #%d: %v", i, err)
		}
		if want := "hello " + in.Name; out.GetMessage() != want {
			t.Fatalf("call #%d: reply = %q, want %q", i, out.GetMessage(), want)
		}
	}
}

// TestCallerRejectsConcurrentUse pins the guard on the real path. A Caller owns
// one scratch buffer and one config, so the second caller must get an error
// rather than a corrupted request body.
//
// The server holds the first call open, so the second arrives while the first
// is genuinely in flight — hoping two goroutines happen to overlap would make
// this pass on a bad build.
func TestCallerRejectsConcurrentUse(t *testing.T) {
	x := newCaller(t, &greeter{unaryDelay: 300 * time.Millisecond})
	ctx := testCtx(t)

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- x.SayHello(ctx, &helloworld.HelloRequest{Name: "slow"}, &helloworld.HelloReply{})
	}()
	<-started
	time.Sleep(50 * time.Millisecond) // the first call is now inside the guard

	err := x.SayHello(ctx, &helloworld.HelloRequest{Name: "fast"}, &helloworld.HelloReply{})
	if !errors.Is(err, pgrpc.ErrCallerInUse) {
		t.Errorf("second concurrent call = %v, want ErrCallerInUse", err)
	}
	if err := <-done; err != nil {
		t.Errorf("the first call failed: %v", err)
	}
}

// TestCallerSendsClientDefaultCallOptions is a regression test with a wire
// assertion, because the defect it covers was invisible from inside the
// process: the Client face sent the client-wide header and the Caller face
// silently sent nothing, from the same *pgrpc.Client and the same option. The
// Caller's config used to start empty, and only the ergonomic entry points ever
// read the client's defaults.
func TestCallerSendsClientDefaultCallOptions(t *testing.T) {
	x := newCallerWithOptions(t, &greeter{},
		pgrpc.WithDefaultCallOptions(pgrpc.WithHeaderString("x-tenant", "acme")))

	out := &helloworld.HelloReply{}
	if err := x.SayHello(testCtx(t), &helloworld.HelloRequest{Name: "world"}, out); err != nil {
		t.Fatalf("SayHello: %v", err)
	}
	if want := "hello world [tenant=acme]"; out.GetMessage() != want {
		t.Errorf("reply = %q, want %q — the client-wide default never reached the server",
			out.GetMessage(), want)
	}
}

// TestClientAndCallerAgreeOnDefaults states the invariant the regression above
// broke: two faces over one Client must put the same metadata on the wire.
func TestClientAndCallerAgreeOnDefaults(t *testing.T) {
	impl := &greeter{}
	addr := serve(t, impl)
	cc := dial(t, addr)
	c := pgrpc.NewClient(cc, pgrpc.WithCodec(protocodec.Codec{}),
		pgrpc.WithDefaultCallOptions(pgrpc.WithHeaderString("x-tenant", "acme")))

	viaClient, err := poseidon.NewGreeterClient(c).SayHello(testCtx(t),
		&helloworld.HelloRequest{Name: "world"})
	if err != nil {
		t.Fatalf("client face: %v", err)
	}

	viaCaller := &helloworld.HelloReply{}
	if err := poseidon.NewGreeterCaller(c).SayHello(testCtx(t),
		&helloworld.HelloRequest{Name: "world"}, viaCaller); err != nil {
		t.Fatalf("caller face: %v", err)
	}

	if viaClient.GetMessage() != viaCaller.GetMessage() {
		t.Errorf("the two faces disagree on the wire: client %q, caller %q",
			viaClient.GetMessage(), viaCaller.GetMessage())
	}
}

// TestUnaryRejectsAMalformedHeaderBeforeSending checks that an option failure
// stops the call rather than silently dropping the header the caller thought
// they had attached.
func TestUnaryRejectsAMalformedHeaderBeforeSending(t *testing.T) {
	c := newClient(t, &greeter{})
	_, err := c.SayHello(testCtx(t), &helloworld.HelloRequest{Name: "x"},
		pgrpc.WithHeaderString("content-type", "application/grpc+json"))
	if err == nil {
		t.Fatal("a reserved header was accepted")
	}
	if !strings.Contains(err.Error(), "reserved") {
		t.Errorf("err = %v, want it to name the reserved key", err)
	}
}

func TestStatusCodeMappingMatchesGrpcGo(t *testing.T) {
	for _, tc := range []struct {
		code codes.Code
		want pgrpc.Code
	}{
		{codes.NotFound, pgrpc.NotFound},
		{codes.AlreadyExists, pgrpc.AlreadyExists},
		{codes.Unauthenticated, pgrpc.Unauthenticated},
		{codes.Unimplemented, pgrpc.Unimplemented},
		{codes.Internal, pgrpc.Internal},
		{codes.Unavailable, pgrpc.Unavailable},
		{codes.DataLoss, pgrpc.DataLoss},
	} {
		t.Run(tc.code.String(), func(t *testing.T) {
			c := newClient(t, &greeter{unaryErr: status.Error(tc.code, "x")})
			_, err := c.SayHello(testCtx(t), &helloworld.HelloRequest{Name: "n"})
			if got := pgrpc.StatusOf(err).Code; got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
			if gotCode, _ := statusOf(err); gotCode != codes.Unknown && gotCode != tc.code {
				t.Logf("grpc-go read the same error as %v", gotCode)
			}
		})
	}
}
