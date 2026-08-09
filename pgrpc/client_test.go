package pgrpc_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lodgvideon/poseidon-http-client/conn"
	"github.com/lodgvideon/poseidon-http-client/grpc"
	"github.com/lodgvideon/protoc-gen-go-poseidon/pgrpc"
)

// fakeInvoker is the substitutability claim made concrete: the unary half can
// be faked with nothing but byte slices. The streaming half cannot, because
// *grpc.Stream has no exported constructor — so it returns an error, which is
// the honest thing a double can do.
type fakeInvoker struct {
	method string
	req    []byte
	md     []conn.HeaderField
	opts   []grpc.CallOption
	resp   []byte
	err    error
	calls  int
}

func (f *fakeInvoker) Invoke(_ context.Context, method string, req []byte,
	md []conn.HeaderField, opts ...grpc.CallOption) ([]byte, error) {
	f.calls++
	f.method, f.req, f.md, f.opts = method, req, md, opts
	return f.resp, f.err
}

func (f *fakeInvoker) InvokeInto(_ context.Context, method string, req, dst []byte,
	md []conn.HeaderField, opts ...grpc.CallOption) ([]byte, error) {
	f.calls++
	f.method, f.md, f.opts = method, md, opts
	f.req = append(f.req[:0], req...)
	if f.err != nil {
		// Mirror poseidon: dst[:0] rather than nil, so a looping caller keeps
		// its buffer. A fake that returned nil here would hide a regression in
		// the code that relies on that property.
		return dst[:0], f.err
	}
	return append(dst, f.resp...), nil
}

func (f *fakeInvoker) NewStream(_ context.Context, _ string,
	_ []conn.HeaderField, _ ...grpc.CallOption) (*grpc.Stream, error) {
	return nil, errors.New("fakeInvoker cannot produce a *grpc.Stream")
}

var _ pgrpc.Invoker = (*fakeInvoker)(nil)

func TestNewClientPanicsWithoutACodec(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("NewClient accepted a nil codec")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "WithCodec") {
			t.Errorf("panic message does not say how to fix it: %v", r)
		}
	}()
	pgrpc.NewClient(&fakeInvoker{})
}

func TestNewClientPanicsOnNilInvoker(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewClient accepted a nil Invoker")
		}
	}()
	pgrpc.NewClient(nil, pgrpc.WithCodec(stubCodec{}))
}

func TestNewClientIgnoresNilOptions(t *testing.T) {
	c := pgrpc.NewClient(&fakeInvoker{}, nil, pgrpc.WithCodec(stubCodec{}), nil)
	if c.Codec() == nil {
		t.Fatal("codec not set")
	}
}

// TestCodecForIsTheSingleResolutionRule pins the precedence in the one place
// that implements it. Two channels where only one is read is how a per-call
// codec silently produces the wrong wire encoding.
func TestCodecForIsTheSingleResolutionRule(t *testing.T) {
	clientCodec := stubCodec{name: "client"}
	callCodec := stubCodec{name: "call"}
	c := pgrpc.NewClient(&fakeInvoker{}, pgrpc.WithCodec(clientCodec))

	if got := c.CodecFor(nil).Name(); got != "client" {
		t.Errorf("CodecFor(nil) = %q, want the client's", got)
	}
	if got := c.CodecFor(&pgrpc.CallConfig{}).Name(); got != "client" {
		t.Errorf("CodecFor(empty) = %q, want the client's", got)
	}
	if got := c.CodecFor(&pgrpc.CallConfig{Codec: callCodec}).Name(); got != "call" {
		t.Errorf("CodecFor(override) = %q, want the call's", got)
	}
}

func TestInvokerIsReturnedUnwrapped(t *testing.T) {
	f := &fakeInvoker{}
	c := pgrpc.NewClient(f, pgrpc.WithCodec(stubCodec{}))
	if c.Invoker() != pgrpc.Invoker(f) {
		t.Error("Invoker() did not return the connection it was given")
	}
}

func TestDefaultCallOptionsAccumulateInOrder(t *testing.T) {
	c := pgrpc.NewClient(&fakeInvoker{},
		pgrpc.WithCodec(stubCodec{}),
		pgrpc.WithDefaultCallOptions(pgrpc.WithHeaderString("a", "1")),
		pgrpc.WithDefaultCallOptions(pgrpc.WithHeaderString("b", "2")),
	)
	if got := len(c.DefaultCallOptions()); got != 2 {
		t.Fatalf("got %d default options, want 2", got)
	}

	var cfg pgrpc.CallConfig
	cfg.Apply(c.DefaultCallOptions()...)
	if cfg.Err != nil {
		t.Fatalf("applying defaults: %v", cfg.Err)
	}
	if len(cfg.MD) != 2 || string(cfg.MD[0].Name) != "a" || string(cfg.MD[1].Name) != "b" {
		t.Errorf("defaults applied out of order: %v", cfg.MD)
	}
}

// TestDefaultMetadataIsNotWrittenThroughByACall is the aliasing invariant at
// the level a user would actually hit it: a shared default plus a per-call
// header, twice, must not have the second call's header land in the first's
// memory.
func TestDefaultMetadataIsNotWrittenThroughByACall(t *testing.T) {
	shared := mdOf(t, "x-tenant", "acme")
	c := pgrpc.NewClient(&fakeInvoker{},
		pgrpc.WithCodec(stubCodec{}),
		pgrpc.WithDefaultCallOptions(pgrpc.WithMetadata(shared)),
	)

	build := func(id string) []conn.HeaderField {
		var cfg pgrpc.CallConfig
		cfg.Apply(c.DefaultCallOptions()...)
		cfg.Apply(pgrpc.WithHeaderString("x-request-id", id))
		if cfg.Err != nil {
			t.Fatalf("resolve: %v", cfg.Err)
		}
		return cfg.MD
	}

	first := build("r-1")
	second := build("r-2")

	if sameArray(first, second) {
		t.Fatal("two calls share a metadata array; one would overwrite the other's header")
	}
	if len(shared) != 1 {
		t.Errorf("the default grew to %d entries", len(shared))
	}
	if string(first[1].Value) != "r-1" || string(second[1].Value) != "r-2" {
		t.Errorf("headers crossed over: %q and %q", first[1].Value, second[1].Value)
	}
}

func TestUnaryInvokerHalfIsFakeable(t *testing.T) {
	f := &fakeInvoker{resp: []byte("pong")}
	var u pgrpc.UnaryInvoker = f

	got, err := u.Invoke(context.Background(), "/svc/M", []byte("ping"), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if string(got) != "pong" {
		t.Errorf("resp = %q, want %q", got, "pong")
	}
	if f.method != "/svc/M" {
		t.Errorf("method = %q", f.method)
	}
}
