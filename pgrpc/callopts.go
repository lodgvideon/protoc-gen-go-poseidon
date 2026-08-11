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
//     on it: metadata set here is handed to poseidon positionally, and
//     grpc.WithMetadata is never used, or every header would ship twice;
//  2. a pass-through list of genuine grpc.CallOptions, forwarded verbatim — so
//     an option poseidon adds in future works here the day it ships, with no
//     release of this module;
//  3. per-call state poseidon has no concept of, currently the Codec.
//
// Unlike poseidon's, this interface is genuinely OPEN: Apply is exported
// deliberately, so a caller outside this package can write their own option. An
// unexported apply would have made it exactly as closed as poseidon's — an
// exported parameter type changes nothing.
type CallOption interface{ Apply(*CallConfig) }

// CallConfig is the resolved per-call configuration.
//
// Its fields are unexported and reached through methods, which is not
// ceremony: the metadata slice has an ownership rule that decides whether an
// append lands in this config's memory or in somebody else's, and a plain
// exported field lets a caller defeat that rule by assignment, silently. The
// methods are the rule.
//
// The zero value is ready to use. It is safe to copy BY VALUE — a copy adopts
// its own metadata on first modification, so two copies never write into one
// array.
type CallConfig struct {
	// md is handed to poseidon POSITIONALLY. Never through grpc.WithMetadata,
	// which would duplicate every header.
	md []conn.HeaderField

	// owner identifies the CallConfig whose modifications produced md's backing
	// array. It is compared against &c, so it answers "is this array mine"
	// rather than "has somebody adopted at some point" — which is what makes a
	// VALUE COPY safe. A bool latch cannot: it copies as true, and both copies
	// then append into one array.
	//
	// The failure that shape produces is not a crash. poseidon's buildHeaders
	// reads md[i] SYNCHRONOUSLY inside NewStream, so one virtual user's header
	// — routinely a credential — ships on another user's request, with no error,
	// no race-detector finding, and no go vet finding, because this struct holds
	// no lock for copylocks to notice.
	owner *CallConfig

	// pass is forwarded verbatim to poseidon's variadic.
	pass []grpc.CallOption
	// passOwner is pass's counterpart to owner, for the same reason.
	passOwner *CallConfig

	// codec overrides the Client's for this call when non-nil.
	codec Codec

	// err latches the first option failure. Options cannot return errors — they
	// are values applied in a loop — and silently dropping a malformed header
	// would send the RPC without the credential the caller thought they had
	// attached.
	err error
}

// Apply applies opts to c in order, latching the first failure, and returns c
// so it chains.
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

// Metadata returns the request metadata.
//
// The slice is this config's own; treat it as READ-ONLY. Appending to it
// directly is the one thing the method surface cannot prevent, and it is the
// thing that puts one call's header on another's.
func (c *CallConfig) Metadata() []conn.HeaderField { return c.md }

// SetMetadata installs pre-built metadata, REPLACING whatever was there.
//
// The slice is installed by reference, which is what makes it cheap, and is
// recorded as borrowed: the next modification copies it first, so this config
// can never write into memory the caller still owns.
//
// LIFETIME. poseidon stores a text value ALIASING the caller's bytes and copies
// them into its HPACK encoder during SendHeaders, inside NewStream. md must not
// be mutated from another goroutine while any call holding it is in flight.
// Metadata built once from constants satisfies that trivially.
func (c *CallConfig) SetMetadata(md []conn.HeaderField) {
	c.md = md
	c.owner = nil // borrowed: not ours until something copies it
}

// AppendField appends one already-built field.
//
// It adopts the metadata first, so the append can never land in an array this
// config does not own. A CallOption written outside this package should reach
// for this rather than for Metadata.
func (c *CallConfig) AppendField(f conn.HeaderField) {
	c.adopt()
	c.md = append(c.md, f)
}

// adopt copies md into memory this config owns, unless it already owns it.
//
// The full-slice expression is what forces a later append to reallocate rather
// than write past the copy.
func (c *CallConfig) adopt() {
	if c.owner == c {
		return
	}
	c.md = append(c.md[:0:0], c.md...)
	c.owner = c
}

// PoseidonOptions returns the poseidon options this call forwards. Read-only,
// for the same reason as Metadata.
func (c *CallConfig) PoseidonOptions() []grpc.CallOption { return c.pass }

// AppendPoseidonOptions forwards poseidon's own call options verbatim.
//
// A nil entry is dropped rather than forwarded. poseidon calls apply on every
// option in the list, so a nil one panics inside NewStream — in poseidon's
// stack, on a value this package handed it, which is the worst place for a
// caller to have to diagnose it. Apply already ignores a nil pgrpc.CallOption;
// this is the same courtesy one layer down.
func (c *CallConfig) AppendPoseidonOptions(opts ...grpc.CallOption) {
	if c.passOwner != c {
		c.pass = append(c.pass[:0:0], c.pass...)
		c.passOwner = c
	}
	for _, o := range opts {
		if o != nil {
			c.pass = append(c.pass, o)
		}
	}
}

// Codec returns the per-call codec override, or nil to inherit the Client's.
// Resolution lives in Client.CodecFor and nowhere else.
func (c *CallConfig) Codec() Codec { return c.codec }

// SetCodec overrides the codec for this call.
func (c *CallConfig) SetCodec(cd Codec) { c.codec = cd }

// Err returns the first option failure, or nil. Every entry point checks it
// before touching the transport.
func (c *CallConfig) Err() error { return c.err }

// Fail latches err as the first failure. Exported so an option written outside
// this package can report one; options return no error of their own.
func (c *CallConfig) Fail(err error) {
	if c.err == nil {
		c.err = err
	}
}

// Reset prepares c for another call, keeping the backing arrays it owns.
//
// Owned metadata is CLEARED rather than truncated, for the reason poseidon's
// own putHeaderScratch gives: the entries point at memory that for gRPC
// routinely means credentials, and a reused buffer is the wrong place to leave
// those reachable past the RPC that carried them.
//
// Borrowed metadata is dropped rather than cleared, because clearing it would
// zero the slice its owner is still holding.
func (c *CallConfig) Reset() {
	if c.owner == c {
		clear(c.md)
		c.md = c.md[:0]
	} else {
		c.md = nil
		c.owner = c // nil is trivially ours: the first append allocates
	}
	if c.passOwner == c {
		clear(c.pass)
		c.pass = c.pass[:0]
	} else {
		c.pass = nil
		c.passOwner = c
	}
	c.codec = nil
	c.err = nil
}

// metadataOption implements WithMetadata.
type metadataOption struct{ md []conn.HeaderField }

func (o metadataOption) Apply(c *CallConfig) {
	// APPEND, not replace. Two doc comments promised appending while the code
	// replaced, and the direction to fix was not a coin toss: replacing lets a
	// per-call option silently strip a client-wide credential, which is the
	// worse failure of the two.
	//
	// The cost is the copy the borrow trick used to avoid. It is paid only when
	// there is already metadata to preserve: against an empty config this is
	// still an install by reference.
	if len(c.md) == 0 && c.owner != c {
		c.md = o.md
		c.owner = nil
		return
	}
	c.adopt()
	c.md = append(c.md, o.md...)
}

// WithMetadata adds pre-built metadata to the call. It is the zero
// metadata-BUILDING path and the one a benchmark should use: build the slice
// once at setup, hold it, pass it on every RPC.
//
// It APPENDS to whatever the Client's defaults already put there. Against an
// otherwise-empty configuration it installs the slice by reference, which is
// what makes it cheap; where it has to merge, it copies first.
//
// It is not zero-allocation: a struct carrying a slice header does not fit in
// an interface word, so the option value itself is boxed on every invocation.
// The allocation-free form is calling SetMetadata on a reusable caller's
// config once, outside the request loop.
//
// It does not bypass validation — poseidon re-validates every field of a
// caller-supplied slice inside NewStream, precisely because such a slice never
// went through AppendMetadata. What is skipped is only the per-call key
// lowercasing and "-bin" base64, which is the work being hoisted to setup.
func WithMetadata(md []conn.HeaderField) CallOption { return metadataOption{md: md} }

// headerOption implements WithHeader and WithHeaderString.
type headerOption struct {
	key   string
	value []byte
}

func (o headerOption) Apply(c *CallConfig) {
	c.adopt()
	// The two-step append is not cosmetic. grpc.AppendMetadata returns
	// (nil, err) on failure, so the idiomatic `md, err = AppendMetadata(md, …)`
	// DESTROYS everything accumulated so far when one entry is bad. Assigning
	// only on success keeps the earlier entries and reports the failure through
	// Err.
	next, err := grpc.AppendMetadata(c.md, o.key, o.value)
	if err != nil {
		c.Fail(err)
		return
	}
	c.md = next
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

func (o passOption) Apply(c *CallConfig) { c.AppendPoseidonOptions(o.opts...) }

// MaxRecvMessageSize caps this call's received message size. It is sugar over
// poseidon's own option, forwarded verbatim — this package cannot reimplement
// it, because grpc.CallOption is closed and the limit is enforced inside
// poseidon's decoder.
func MaxRecvMessageSize(n int) CallOption {
	return passOption{opts: []grpc.CallOption{grpc.MaxRecvMessageSize(n)}}
}

// WithPoseidonCallOptions forwards poseidon's own call options verbatim. It is
// the escape hatch that keeps this package from becoming a bottleneck when
// poseidon adds an option this module has not wrapped yet.
//
// DO NOT pass grpc.WithMetadata through it. Metadata is handed to poseidon
// positionally, and the same slice arriving through the option tail as well
// duplicates every header on the wire, because buildHeaders walks both lists.
//
// Generated code never emits this; users do.
func WithPoseidonCallOptions(opts ...grpc.CallOption) CallOption {
	return passOption{opts: opts}
}

// codecOption implements WithCallCodec.
type codecOption struct{ c Codec }

func (o codecOption) Apply(c *CallConfig) { c.SetCodec(o.c) }

// WithCallCodec overrides the codec for one call — for the single method whose
// message type is hand-rolled, or to A/B a vtprotobuf codec against the
// reflection one under load without standing up a second client.
func WithCallCodec(cd Codec) CallOption { return codecOption{c: cd} }
