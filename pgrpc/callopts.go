package pgrpc

import (
	"github.com/lodgvideon/poseidon-http-client/conn"
	"github.com/lodgvideon/poseidon-http-client/grpc"
)

// CallOption customises a single RPC.
//
// This type exists because poseidon's grpc.CallOption is a CLOSED interface:
// its only method, apply(*callOptions), is unexported, so no package outside
// poseidon can implement one. poseidon exports two constructors today —
// grpc.MaxRecvMessageSize and grpc.WithMetadata — and may add more. This
// package can forward those; it cannot construct a new KIND of poseidon option.
//
// pgrpc.CallOption is therefore a superset, carrying three things poseidon has
// no single channel for:
//
//  1. request metadata. poseidon accepts metadata BOTH positionally and through
//     grpc.WithMetadata, and its buildHeaders walks the two lists without
//     concatenating them. This package must therefore pick ONE channel and stay
//     on it: WithMetadata sets CallConfig.MD, which is handed to poseidon
//     positionally. The same slice must never also go through
//     grpc.WithMetadata, or every header ships twice;
//  2. a pass-through list of genuine grpc.CallOptions, forwarded verbatim — so
//     an option poseidon adds in future works here the day it ships, with no
//     release of this module;
//  3. per-call state poseidon has no concept of, currently the Codec.
//
// Unlike poseidon's, this interface is genuinely OPEN: Apply is exported
// deliberately, so a caller outside this package can write their own option. An
// unexported apply would have made it exactly as closed as poseidon's — an
// exported parameter type changes nothing. The cost is that CallConfig's field
// set is part of the public API: adding a field is a minor release, removing
// one is major.
type CallOption interface{ Apply(*CallConfig) }

// CallConfig is the resolved per-call configuration. It is exported and
// directly mutable so that a caller can set it once, outside a request loop,
// instead of resolving a variadic option list on every RPC.
//
// The zero value is ready to use.
type CallConfig struct {
	// MD is the metadata handed to poseidon POSITIONALLY — never through
	// grpc.WithMetadata, which would duplicate every header (see CallOption).
	//
	// Assigning this field directly is supported, and is the allocation-free
	// way to attach metadata that never changes. Doing so marks the slice as
	// borrowed: the first option that needs to append copies it first, so this
	// config can never write into memory the assigning caller still owns.
	MD []conn.HeaderField

	// Pass is the verbatim grpc.CallOption list forwarded to poseidon's
	// variadic. grpc.WithMetadata is deliberately never put here.
	Pass []grpc.CallOption

	// Codec overrides the Client's codec for this call when non-nil. Nil means
	// inherit. This precedence rule is implemented in exactly one place, and it
	// is not here.
	Codec Codec

	// Err latches the first option failure. Options cannot return errors — they
	// are values applied in a loop — and silently dropping a malformed header
	// would send the RPC without the credential the caller thought they had
	// attached. Every entry point checks this before touching the transport.
	Err error

	// mdOwned reports whether MD's backing array belongs to this config.
	//
	// This is the mechanism behind the copy rule, and it is not a nicety.
	// poseidon's buildHeaders reads md[i] SYNCHRONOUSLY inside NewStream, so
	// appending into an array another goroutine's in-flight RPC is reading
	// ships one virtual user's header — routinely a credential — on another
	// user's request. The zero value is false, which is the safe direction: a
	// config whose MD was assigned by hand is treated as borrowed until
	// something copies it.
	mdOwned bool
}

// Apply applies opts to c in order, latching the first failure in c.Err, and
// returns c so it chains.
//
// This is the bridge that makes the two configuration mechanisms interoperate.
// Without it, a caller holding a CallConfig has no way to APPLY an option and
// must assign fields by hand — bypassing WithHeader's failure-preserving append
// and the borrow tracking on MD. Write
//
//	caller.Config().Apply(pgrpc.WithHeaderString("x-tenant", tenant))
//
// rather than assigning MD yourself.
//
// On a reusable caller Apply runs once, at setup, so the per-option interface
// boxing is paid once per virtual user rather than once per RPC.
func (c *CallConfig) Apply(opts ...CallOption) *CallConfig {
	for _, o := range opts {
		if o == nil {
			continue
		}
		o.Apply(c)
	}
	return c
}

// Reset prepares c for another call, keeping the backing arrays it owns.
//
// Owned metadata is cleared rather than merely truncated, for the reason
// poseidon's own putHeaderScratch gives: the entries point at memory that for
// gRPC routinely means credentials, and a reused buffer is the wrong place to
// leave those reachable past the RPC that carried them.
//
// Borrowed metadata is dropped rather than cleared. Clearing it would zero the
// slice its owner is still holding.
//
// Reset does not reinstall any default metadata; option resolution does that.
func (c *CallConfig) Reset() {
	if c.mdOwned {
		clear(c.MD)
		c.MD = c.MD[:0]
	} else {
		c.MD = nil
		c.mdOwned = true // nil is trivially ours: the first append allocates.
	}
	clear(c.Pass)
	c.Pass = c.Pass[:0]
	c.Codec = nil
	c.Err = nil
}

// AdoptMetadata copies MD into memory this config owns, if it does not own it
// already, using a full-slice expression so that a later append reallocates
// instead of writing past the copy.
//
// Every path that appends to MD must call this first. It is exported because
// option resolution lives outside this file and because a caller writing their
// own CallOption needs it for exactly the same reason.
func (c *CallConfig) AdoptMetadata() {
	if c.mdOwned {
		return
	}
	c.MD = append(c.MD[:0:0], c.MD...)
	c.mdOwned = true
}

// setErr latches the first failure.
func (c *CallConfig) setErr(err error) {
	if c.Err == nil {
		c.Err = err
	}
}

// metadataOption implements WithMetadata.
type metadataOption struct{ md []conn.HeaderField }

func (o metadataOption) Apply(c *CallConfig) {
	c.MD = o.md
	// Borrowed: the slice belongs to whoever built it. The next append copies.
	c.mdOwned = false
}

// WithMetadata installs pre-built metadata. It is the zero metadata-BUILDING
// path and the one a benchmark should use: build the slice once at setup, hold
// it, pass it on every RPC.
//
// It is not zero-allocation. A struct carrying a slice header does not fit in
// an interface word, so the option value itself is boxed on every invocation —
// one heap allocation per RPC on the ergonomic path. The genuinely
// allocation-free form is assigning CallConfig.MD once, outside the request
// loop.
//
// The slice is installed BY REFERENCE, which is what makes it cheap. That is
// safe because it is recorded as borrowed: any later append copies first.
//
// It does not bypass validation — poseidon re-validates every field of a
// caller-supplied slice inside NewStream, precisely because such a slice never
// went through AppendMetadata. What is skipped is only the per-call key
// lowercasing and "-bin" base64, which is the work being hoisted to setup.
//
// LIFETIME. poseidon stores a text value ALIASING the caller's bytes and copies
// them into its HPACK encoder during SendHeaders, inside NewStream. So md must
// not be mutated from another goroutine while any call holding it is in flight.
// Metadata built once from constants satisfies that trivially.
func WithMetadata(md []conn.HeaderField) CallOption { return metadataOption{md: md} }

// headerOption implements WithHeader and WithHeaderString.
type headerOption struct {
	key   string
	value []byte
}

func (o headerOption) Apply(c *CallConfig) {
	c.AdoptMetadata()
	// The two-step append is not cosmetic. grpc.AppendMetadata returns
	// (nil, err) on failure, so the idiomatic `md, err = AppendMetadata(md, …)`
	// DESTROYS everything accumulated so far when one entry is bad. Assigning
	// only on success keeps the earlier entries and reports the failure through
	// CallConfig.Err.
	next, err := grpc.AppendMetadata(c.MD, o.key, o.value)
	if err != nil {
		c.setErr(err)
		return
	}
	c.MD = next
}

// WithHeader appends one entry through grpc.AppendMetadata, so the key is
// lowercased, a "-bin" key is base64-encoded, and the reserved-key and
// field-syntax checks run. None of that is bypassable.
//
// It allocates. On a hot path prefer WithMetadata.
func WithHeader(key string, value []byte) CallOption {
	return headerOption{key: key, value: value}
}

// WithHeaderString is WithHeader for a text value.
func WithHeaderString(key, value string) CallOption {
	return headerOption{key: key, value: []byte(value)}
}

// passOption implements MaxRecvMessageSize and WithPoseidonCallOptions.
type passOption struct{ opts []grpc.CallOption }

func (o passOption) Apply(c *CallConfig) { c.Pass = append(c.Pass, o.opts...) }

// MaxRecvMessageSize caps this call's received message size. It is sugar over
// poseidon's own option, forwarded through Pass — this package cannot
// reimplement it, because grpc.CallOption is closed and the limit is enforced
// inside poseidon's decoder.
//
// It costs about three allocations rather than one: this package's option box,
// poseidon's int boxed into grpc.CallOption, and the append onto a nil Pass. A
// reusable caller pays all three once, at setup, not per call.
func MaxRecvMessageSize(n int) CallOption {
	return passOption{opts: []grpc.CallOption{grpc.MaxRecvMessageSize(n)}}
}

// WithPoseidonCallOptions forwards poseidon's own call options verbatim. It is
// the escape hatch that keeps this package from becoming a bottleneck when
// poseidon adds an option this module has not wrapped yet.
//
// DO NOT pass grpc.WithMetadata through it. CallConfig.MD is the single
// metadata channel and is handed to poseidon positionally; the same slice
// arriving through the option tail as well duplicates every header on the wire,
// because buildHeaders walks both lists.
//
// Generated code never emits this; users do.
func WithPoseidonCallOptions(opts ...grpc.CallOption) CallOption {
	return passOption{opts: opts}
}

// codecOption implements WithCallCodec.
type codecOption struct{ c Codec }

func (o codecOption) Apply(c *CallConfig) { c.Codec = o.c }

// WithCallCodec overrides the codec for one call — for the single method whose
// message type is hand-rolled, or to A/B a vtprotobuf codec against the
// reflection one under load without standing up a second client.
func WithCallCodec(cd Codec) CallOption { return codecOption{c: cd} }
