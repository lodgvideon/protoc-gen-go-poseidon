package pgrpc

import (
	"context"

	"github.com/lodgvideon/poseidon-http-client/conn"
	"github.com/lodgvideon/poseidon-http-client/grpc"
)

// UnaryInvoker is the substitutable half of the connection seam. It traffics
// only in []byte and []conn.HeaderField, both of which a fake can produce, so a
// unary-only test double is straightforward.
//
// InvokeInto is what this package actually calls; Invoke is kept because it is
// what poseidon's own grpc.Invoker declares, and dropping it would stop a
// user-written decorator that forwards only Invoke from satisfying this
// interface. This is the one place pgrpc.Invoker is a strict SUPERSET of
// poseidon's, and the second reason it is declared here rather than aliased.
type UnaryInvoker interface {
	Invoke(ctx context.Context, method string, req []byte,
		md []conn.HeaderField, opts ...grpc.CallOption) ([]byte, error)
	InvokeInto(ctx context.Context, method string, req, dst []byte,
		md []conn.HeaderField, opts ...grpc.CallOption) ([]byte, error)
}

// StreamInvoker is the half that cannot be faked.
//
// NewStream returns the CONCRETE *grpc.Stream rather than an interface, because
// that is what poseidon returns and Go has no covariant returns; re-typing it
// here would be a lie. The consequence is that a double can only hand back a
// stream it obtained from a real *grpc.ClientConn — grpc.Stream's fields are
// all unexported and poseidon exposes no constructor, so a hand-built
// &grpc.Stream{} nil-panics on first use. Streaming tests need a real loopback
// server. That is a poseidon limitation, not a choice made here.
type StreamInvoker interface {
	NewStream(ctx context.Context, method string,
		md []conn.HeaderField, opts ...grpc.CallOption) (*grpc.Stream, error)
}

// Invoker is what a Client talks to. *grpc.ClientConn satisfies it
// structurally; poseidon knows nothing about this package.
//
// What it usefully substitutes: a pool that picks a connection per call, a
// decorator that injects metadata or counts calls or injects faults, and a
// unary-only fake through UnaryInvoker alone.
type Invoker interface {
	UnaryInvoker
	StreamInvoker
}

// Compile-time proof that poseidon's connection satisfies this interface. If
// poseidon ever changes Invoke's or NewStream's signature, this line is where
// the module breaks — loudly, at build time — rather than in generated code
// scattered across a user's repository.
//
// poseidon v0.12.0 declares its own grpc.Invoker with exactly this method set
// and asserts it itself, so this assertion is redundant with poseidon's. It is
// kept deliberately, and the interface is declared locally rather than aliased
// to grpc.Invoker, because the UnaryInvoker/StreamInvoker split above is the
// substitutable/non-substitutable boundary this package's testing story rests
// on — an alias would leave Invoker unrelated to its own two halves.
var _ Invoker = (*grpc.ClientConn)(nil)

// Client is the per-generated-client state: which connection, which codec, and
// which options apply before the caller's.
//
// It is immutable after construction and safe for concurrent use by any number
// of goroutines, which is what lets one generated client serve every virtual
// user in a load generator.
//
// This package deliberately does NOT wrap grpc.Dial. poseidon's Options and
// ConnOptions surface is large, evolving and genuinely useful; a caller dials
// poseidon directly and hands the *grpc.ClientConn here.
type Client struct {
	cc      Invoker
	codec   Codec
	defOpts []CallOption
}

// NewClient builds the state a generated client holds. Both a connection and a
// codec are required.
//
// It panics rather than returning an error, because a generated constructor has
// no error return, and both mistakes are programming errors that would
// otherwise surface as a nil dereference on the first RPC — arbitrarily far in
// time and stack from their cause.
//
// There is no default codec, because this package must not import protobuf.
// Forgetting WithCodec therefore has to fail loudly at construction. To remove
// the need to remember it at all, the generator's default_codec option makes
// the GENERATED constructor supply protocodec.Codec{} — which is free of the
// no-protobuf constraint, because the generated file's own message package
// already links the protobuf runtime.
func NewClient(cc Invoker, opts ...ClientOption) *Client {
	if cc == nil {
		panic("pgrpc: NewClient got a nil Invoker; pass the *grpc.ClientConn you dialled")
	}
	c := &Client{cc: cc}
	for _, o := range opts {
		if o != nil {
			o(c)
		}
	}
	if c.codec == nil {
		panic("pgrpc: no Codec configured; pass pgrpc.WithCodec(protocodec.Codec{}) or a vtcodec.Codec")
	}
	return c
}

// Invoker returns the connection. Generated callers pass it explicitly, so one
// Client can fan across several connections chosen per RPC by a pool.
func (c *Client) Invoker() Invoker { return c.cc }

// Codec returns the client-wide codec. Per-call overrides are resolved by
// CodecFor, not here.
func (c *Client) Codec() Codec { return c.codec }

// DefaultCallOptions returns the options applied before every call's own.
//
// The slice is shared, not copied: it is written once at construction and read
// concurrently thereafter. Callers must not modify it.
func (c *Client) DefaultCallOptions() []CallOption { return c.defOpts }

// CodecFor is THE codec-resolution rule, used by every entry point in this
// package and by generated code: a non-nil cfg.Codec overrides the Client's for
// that call, nil means inherit.
//
// Nothing else may resolve a codec. Two channels where only one is read is
// exactly how a per-call codec silently produces the wrong wire encoding.
//
// It is exported because a generated caller holding a prepared CallConfig calls
// the resolved-config entry points directly and must pass a codec; an
// unexported helper would push it towards Codec(), which silently ignores
// cfg.Codec.
func (c *Client) CodecFor(cfg *CallConfig) Codec {
	if cfg != nil && cfg.Codec() != nil {
		return cfg.Codec()
	}
	return c.codec
}

// NewCallConfig returns a CallConfig with c's default call options already
// applied.
//
// It exists because the resolved-config entry points take a config the caller
// prepared, and a caller that built one with `var cfg CallConfig` silently got
// NONE of the client's defaults — the Client face sent them and the Caller face
// did not, from the same *Client and the same option. Generated callers are
// constructed through this.
//
// Returning by value is safe: the copy handed back does not own the metadata
// the local one adopted, so its first modification copies rather than writing
// into an array this function no longer tracks.
func NewCallConfig(c *Client) CallConfig {
	var cfg CallConfig
	cfg.Apply(c.defOpts...)
	return cfg
}
