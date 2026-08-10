package pgrpc_test

import (
	"context"
	"errors"
	"testing"
	"unsafe"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/lodgvideon/protoc-gen-go-poseidon/pgrpc"
	"github.com/lodgvideon/protoc-gen-go-poseidon/pgrpc/protocodec"
)

const testMethod = "/helloworld.Greeter/SayHello"

// errCodec fails on demand, so the two codec failure paths can be reached
// without a malformed message.
type errCodec struct {
	marshalErr   error
	unmarshalErr error
}

func (c errCodec) MarshalAppend(dst []byte, m any) ([]byte, error) {
	if c.marshalErr != nil {
		// Grow before failing, the way a codec that got partway through a
		// large message would. MarshalAppend's contract leaves the returned
		// slice unspecified on error, but the ARRAY is still the caller's, and
		// a looping caller must not lose that growth.
		return append(dst, make([]byte, errPathGrowth)...), c.marshalErr
	}
	return protocodec.Codec{}.MarshalAppend(dst, m)
}

func (c errCodec) Unmarshal(src []byte, m any) error {
	if c.unmarshalErr != nil {
		return c.unmarshalErr
	}
	return protocodec.Codec{}.Unmarshal(src, m)
}

func (errCodec) Name() string { return "err" }

// wireOf marshals a message the way a server would have.
func wireOf(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}
	return b
}

func TestUnaryRoundTrip(t *testing.T) {
	f := &fakeInvoker{resp: wireOf(t, wrapperspb.String("pong"))}
	c := pgrpc.NewClient(f, pgrpc.WithCodec(protocodec.Codec{}))

	out := &wrapperspb.StringValue{}
	if err := pgrpc.Unary(context.Background(), c, &pgrpc.CallConfig{},
		testMethod, wrapperspb.String("ping"), out, nil, nil); err != nil {
		t.Fatalf("Unary: %v", err)
	}

	if out.GetValue() != "pong" {
		t.Errorf("out = %q, want %q", out.GetValue(), "pong")
	}
	if f.method != testMethod {
		t.Errorf("method = %q, want %q", f.method, testMethod)
	}
	if want := wireOf(t, wrapperspb.String("ping")); string(f.req) != string(want) {
		t.Errorf("request bytes = % x, want % x", f.req, want)
	}
}

// TestUnaryShortCircuitsOnConfigError is the point of latching option failures.
// A malformed header must not reach the wire as a call MISSING the credential
// the caller thought they attached.
func TestUnaryShortCircuitsOnConfigError(t *testing.T) {
	f := &fakeInvoker{}
	c := pgrpc.NewClient(f, pgrpc.WithCodec(protocodec.Codec{}))

	cfg := &pgrpc.CallConfig{}
	cfg.Apply(pgrpc.WithHeaderString("content-type", "nope")) // reserved
	if cfg.Err() == nil {
		t.Fatal("setup: expected a config error")
	}

	err := pgrpc.Unary(context.Background(), c, cfg, testMethod,
		wrapperspb.String("x"), &wrapperspb.StringValue{}, nil, nil)
	if !errors.Is(err, cfg.Err()) {
		t.Errorf("err = %v, want the latched config error", err)
	}
	if f.calls != 0 {
		t.Errorf("the transport was touched %d times despite a config error", f.calls)
	}
}

// TestUnaryDoesNotOpenAStreamOnMarshalFailure pins the ordering. A half-opened
// stream on a marshal bug leaves the server waiting out the whole deadline for
// a request that is never coming.
func TestUnaryDoesNotOpenAStreamOnMarshalFailure(t *testing.T) {
	boom := errors.New("cannot encode")
	f := &fakeInvoker{}
	c := pgrpc.NewClient(f, pgrpc.WithCodec(errCodec{marshalErr: boom}))

	err := pgrpc.Unary(context.Background(), c, &pgrpc.CallConfig{}, testMethod,
		wrapperspb.String("x"), &wrapperspb.StringValue{}, nil, nil)

	if f.calls != 0 {
		t.Errorf("the transport was touched %d times after a marshal failure", f.calls)
	}
	var ce *pgrpc.CodecError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %T, want *CodecError", err)
	}
	if ce.Op != pgrpc.OpMarshal {
		t.Errorf("Op = %v, want OpMarshal", ce.Op)
	}
	if !errors.Is(err, boom) {
		t.Error("the codec's own cause was lost")
	}
	var st *pgrpc.Status
	if !errors.As(err, &st) || st.Code != pgrpc.Internal {
		t.Errorf("did not classify as Internal: %v", err)
	}
}

func TestUnaryReportsUnmarshalFailure(t *testing.T) {
	boom := errors.New("truncated")
	f := &fakeInvoker{resp: wireOf(t, wrapperspb.String("pong"))}
	c := pgrpc.NewClient(f, pgrpc.WithCodec(errCodec{unmarshalErr: boom}))

	err := pgrpc.Unary(context.Background(), c, &pgrpc.CallConfig{}, testMethod,
		wrapperspb.String("x"), &wrapperspb.StringValue{}, nil, nil)

	var ce *pgrpc.CodecError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %T, want *CodecError", err)
	}
	if ce.Op != pgrpc.OpUnmarshal {
		t.Errorf("Op = %v, want OpUnmarshal", ce.Op)
	}
}

// TestUnaryPassesAStatusThroughUnchanged is the contract that lets a caller
// treat a generated client exactly like the connection underneath it.
func TestUnaryPassesAStatusThroughUnchanged(t *testing.T) {
	want := &pgrpc.Status{Code: pgrpc.NotFound, Message: "no such greeter"}
	f := &fakeInvoker{err: want}
	c := pgrpc.NewClient(f, pgrpc.WithCodec(protocodec.Codec{}))

	err := pgrpc.Unary(context.Background(), c, &pgrpc.CallConfig{}, testMethod,
		wrapperspb.String("x"), &wrapperspb.StringValue{}, nil, nil)

	var got *pgrpc.Status
	if !errors.As(err, &got) {
		t.Fatalf("err = %T, want *Status", err)
	}
	if got != want {
		t.Error("the status was wrapped or copied; it must pass through unchanged")
	}
}

func TestUnaryPassesTransportErrorsThroughVerbatim(t *testing.T) {
	boom := errors.New("connection reset")
	f := &fakeInvoker{err: boom}
	c := pgrpc.NewClient(f, pgrpc.WithCodec(protocodec.Codec{}))

	err := pgrpc.Unary(context.Background(), c, &pgrpc.CallConfig{}, testMethod,
		wrapperspb.String("x"), &wrapperspb.StringValue{}, nil, nil)
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the transport error verbatim", err)
	}
	var ce *pgrpc.CodecError
	if errors.As(err, &ce) {
		t.Error("a transport error was dressed up as a CodecError")
	}
}

// TestUnaryReusesCallerBuffers is the whole reason the resolved-config form
// takes scratch pointers. Once warm, neither buffer may be reallocated.
func TestUnaryReusesCallerBuffers(t *testing.T) {
	f := &fakeInvoker{resp: wireOf(t, wrapperspb.String("pong"))}
	c := pgrpc.NewClient(f, pgrpc.WithCodec(protocodec.Codec{}))

	var reqBuf, respBuf []byte
	out := &wrapperspb.StringValue{}
	call := func() {
		t.Helper()
		if err := pgrpc.Unary(context.Background(), c, &pgrpc.CallConfig{},
			testMethod, wrapperspb.String("ping"), out, &reqBuf, &respBuf); err != nil {
			t.Fatalf("Unary: %v", err)
		}
	}

	call() // warm up: the buffers are allocated here
	call()
	reqArr, respArr := unsafe.SliceData(reqBuf), unsafe.SliceData(respBuf)

	for range 5 {
		call()
	}
	if unsafe.SliceData(reqBuf) != reqArr {
		t.Error("the request buffer was reallocated on a steady-state call")
	}
	if unsafe.SliceData(respBuf) != respArr {
		t.Error("the response buffer was reallocated on a steady-state call")
	}
	if out.GetValue() != "pong" {
		t.Errorf("out = %q after reuse", out.GetValue())
	}
}

// TestUnaryKeepsCallerBuffersOnFailure covers the paths a looping caller hits
// under load: a failure must not cost it the buffer growth it already paid for.
//
// The assertion is on CAPACITY, not on the array's identity. Both the transport
// and the codec can grow the buffer and only then fail — poseidon's InvokeInto
// hands the grown array back as dst[:0] for exactly this reason — so the growth
// arrives in a REALLOCATED array, and a test comparing pointers would pass
// whether or not the result was stored. Capacity is what discriminates.
func TestUnaryKeepsCallerBuffersOnFailure(t *testing.T) {
	f := &fakeInvoker{resp: wireOf(t, wrapperspb.String("pong"))}
	c := pgrpc.NewClient(f, pgrpc.WithCodec(protocodec.Codec{}))

	reqBuf := make([]byte, 0, 8)
	respBuf := make([]byte, 0, 8)

	f.err = errors.New("transport down")
	err := pgrpc.Unary(context.Background(), c, &pgrpc.CallConfig{}, testMethod,
		wrapperspb.String("ping"), &wrapperspb.StringValue{}, &reqBuf, &respBuf)
	if err == nil {
		t.Fatal("expected the transport error")
	}
	if cap(respBuf) < errPathGrowth {
		t.Errorf("response buffer cap = %d after a transport error, want >= %d — the growth was discarded",
			cap(respBuf), errPathGrowth)
	}
	if len(respBuf) != 0 {
		t.Errorf("response buffer len = %d after a failure, want 0", len(respBuf))
	}

	f.err = nil
	c2 := pgrpc.NewClient(f, pgrpc.WithCodec(errCodec{marshalErr: errors.New("nope")}))
	if err := pgrpc.Unary(context.Background(), c2, &pgrpc.CallConfig{}, testMethod,
		wrapperspb.String("ping"), &wrapperspb.StringValue{}, &reqBuf, &respBuf); err == nil {
		t.Fatal("expected the marshal error")
	}
	if cap(reqBuf) < errPathGrowth {
		t.Errorf("request buffer cap = %d after a marshal error, want >= %d — the growth was discarded",
			cap(reqBuf), errPathGrowth)
	}
	if len(reqBuf) != 0 {
		t.Errorf("request buffer len = %d after a marshal failure, want 0", len(reqBuf))
	}
}

func TestUnaryOptsAppliesDefaultsThenPerCall(t *testing.T) {
	f := &fakeInvoker{resp: wireOf(t, wrapperspb.String("pong"))}
	c := pgrpc.NewClient(f,
		pgrpc.WithCodec(protocodec.Codec{}),
		pgrpc.WithDefaultCallOptions(pgrpc.WithHeaderString("x-tenant", "acme")),
	)

	err := pgrpc.UnaryOpts(context.Background(), c, testMethod,
		wrapperspb.String("ping"), &wrapperspb.StringValue{},
		pgrpc.WithHeaderString("x-request-id", "r-1"))
	if err != nil {
		t.Fatalf("UnaryOpts: %v", err)
	}

	if len(f.md) != 2 {
		t.Fatalf("the call carried %d metadata entries, want 2", len(f.md))
	}
	if string(f.md[0].Name) != "x-tenant" || string(f.md[1].Name) != "x-request-id" {
		t.Errorf("metadata order wrong: %q then %q", f.md[0].Name, f.md[1].Name)
	}
}

func TestUnaryOptsReportsOptionFailureWithoutCalling(t *testing.T) {
	f := &fakeInvoker{}
	c := pgrpc.NewClient(f, pgrpc.WithCodec(protocodec.Codec{}))

	err := pgrpc.UnaryOpts(context.Background(), c, testMethod,
		wrapperspb.String("x"), &wrapperspb.StringValue{},
		pgrpc.WithHeaderString("te", "trailers")) // reserved
	if err == nil {
		t.Fatal("a reserved header was accepted")
	}
	if f.calls != 0 {
		t.Errorf("the transport was touched %d times", f.calls)
	}
}

// TestUnaryUsesThePerCallCodec guards the reason Unary takes no codec
// parameter: a parameter would shadow cfg.Codec and make WithCallCodec produce
// the wrong wire encoding with no diagnostic.
func TestUnaryUsesThePerCallCodec(t *testing.T) {
	boom := errors.New("per-call codec ran")
	f := &fakeInvoker{resp: wireOf(t, wrapperspb.String("pong"))}
	c := pgrpc.NewClient(f, pgrpc.WithCodec(protocodec.Codec{}))

	cfg := &pgrpc.CallConfig{}
	cfg.Apply(pgrpc.WithCallCodec(errCodec{marshalErr: boom}))

	err := pgrpc.Unary(context.Background(), c, cfg, testMethod,
		wrapperspb.String("x"), &wrapperspb.StringValue{}, nil, nil)
	if !errors.Is(err, boom) {
		t.Errorf("err = %v; the per-call codec was ignored", err)
	}
}
