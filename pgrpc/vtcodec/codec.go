// Package vtcodec implements pgrpc.Codec over vtprotobuf's generated methods,
// falling back for messages vtprotobuf did not generate for.
//
// The vtprotobuf methods are found structurally, by interface probe, so this
// package does not import vtprotobuf and adds no module dependency of its own.
package vtcodec

import (
	"fmt"
	"slices"

	"github.com/lodgvideon/protoc-gen-go-poseidon/pgrpc"
	"github.com/lodgvideon/protoc-gen-go-poseidon/pgrpc/protocodec"
)

// vtMarshaler is the size-then-fill shape vtprotobuf generates. It is preferred
// over MarshalVT() ([]byte, error), which allocates its own buffer — exactly
// the allocation the append shape exists to eliminate.
//
// MarshalToSizedBufferVT fills its argument BACKWARDS from the end, so the
// slice handed to it must be EXACTLY SizeVT() bytes long, not merely large
// enough. Getting that wrong silently truncates the message from the front.
type vtMarshaler interface {
	SizeVT() int
	MarshalToSizedBufferVT(dst []byte) (int, error)
}

// vtMarshalerStrict is the same shape emitted by vtprotobuf's `strict` feature,
// which names the method MarshalToSizedBufferVTStrict. Probing only the
// non-strict name would silently miss every strict-generated schema and send it
// down the fallback path.
type vtMarshalerStrict interface {
	SizeVT() int
	MarshalToSizedBufferVTStrict(dst []byte) (int, error)
}

// vtUnmarshaler is vtprotobuf's parse entry point.
//
// UnmarshalVT is gogo-derived and MERGE-shaped: it appends to repeated fields
// and leaves absent scalars alone. pgrpc.Codec.Unmarshal is contractually
// reset-shaped, so a reset must run first. Skipping it makes every response on
// a reused out message accumulate the previous one's repeated fields — a bug
// that appears only under reuse, which is precisely the load-generator path.
//
// UnmarshalVTUnsafe is deliberately not probed: it aliases the input buffer
// into the message, and this package hands it memory that the next receive
// overwrites.
type vtUnmarshaler interface{ UnmarshalVT(b []byte) error }

// The reset probes, kept separate from the parse probe on purpose.
//
// ResetVT is emitted by vtprotobuf's `pool` feature ONLY, and only for messages
// opted in with `option (vtproto.mempool) = true` or an explicit `pool=` plugin
// parameter. On a canonical `features=marshal+unmarshal+size` schema there is
// no ResetVT at all — so folding it into the vtUnmarshaler probe would make the
// fast path silently never match. Reset() is what protoc-gen-go emits for every
// message, and is the usual answer.
type (
	vtResetter interface{ ResetVT() }
	resetter   interface{ Reset() }
)

// Codec marshals with vtprotobuf where the message supports it and falls back
// to Fallback otherwise, so a partly-vt-generated schema still works.
//
// The zero value is ready to use and falls back to protocodec.
type Codec struct {
	// Fallback handles messages vtprotobuf did not generate for. Nil means
	// protocodec.Codec{}.
	//
	// DEPENDENCY CONSEQUENCE: that default makes this package import
	// protocodec, and therefore link google.golang.org/protobuf, which forfeits
	// "a vt-only user links no reflection code". It is still the right default
	// — a zero value that nil-panics on the first RPC is a worse failure than a
	// linked package, and the fallback is reached more often than expected
	// because the strict and pool feature sets change which methods exist. A
	// user who must keep the property sets Fallback to a codec that returns an
	// error for everything.
	Fallback pgrpc.Codec
}

// defaultFallback is boxed once at init rather than per call. Returning
// protocodec.Codec{} directly from fallback below converts a two-field struct
// to an interface at every fallback, which escapes — one allocation on a path
// that a partly-vt-generated schema takes for every message vtprotobuf skipped.
var defaultFallback pgrpc.Codec = protocodec.Codec{}

// fallback returns the codec used for messages vtprotobuf did not generate for.
func (c Codec) fallback() pgrpc.Codec {
	if c.Fallback != nil {
		return c.Fallback
	}
	return defaultFallback
}

// MarshalAppend appends the wire encoding of m to dst, using vtprotobuf's
// generated methods when m has them.
func (c Codec) MarshalAppend(dst []byte, m any) ([]byte, error) {
	var n int
	var strict bool
	switch vm := m.(type) {
	case vtMarshaler:
		n = vm.SizeVT()
	case vtMarshalerStrict:
		n, strict = vm.SizeVT(), true
	default:
		return c.fallback().MarshalAppend(dst, m)
	}

	// The size and the strictness are carried out of the switch as plain
	// values, and the fill method is re-resolved below, rather than binding
	// vm.MarshalToSizedBufferVT to a func variable inside the switch. A method
	// value on an interface receiver is a closure over that receiver, and this
	// is the hot path of a package whose entire premise is not allocating on
	// it.
	dst = slices.Grow(dst, n)
	end := len(dst) + n
	buf := dst[len(dst):end:end]

	var written int
	var err error
	if strict {
		written, err = m.(vtMarshalerStrict).MarshalToSizedBufferVTStrict(buf)
	} else {
		written, err = m.(vtMarshaler).MarshalToSizedBufferVT(buf)
	}
	if err != nil {
		return dst, err
	}
	// MarshalToSizedBufferVT fills BACKWARDS from the END of its argument. A
	// short write therefore leaves the payload at the END of the buffer, not
	// the front — so returning dst[:len(dst)+written] would hand back `written`
	// bytes of UNINITIALISED memory, over which the framing layer would stamp a
	// valid length prefix, and the server would decode junk with no error
	// raised anywhere. Realistic trigger: a request fixture mutated by another
	// goroutine between SizeVT() and the fill. Reject it rather than mis-slice
	// it.
	if written != n {
		return dst, fmt.Errorf("vtcodec: %T wrote %d of %d bytes (message mutated during marshal?)", m, written, n)
	}
	return dst[:end], nil
}

// Unmarshal parses src into m, discarding m's previous contents.
func (c Codec) Unmarshal(src []byte, m any) error {
	vu, ok := m.(vtUnmarshaler)
	if !ok {
		return c.fallback().Unmarshal(src, m)
	}
	// The reset is found separately from the parse probe — see the comment on
	// vtResetter for why requiring ResetVT here would disable the fast path on
	// every schema that did not opt into pooling.
	switch r := m.(type) {
	case vtResetter:
		r.ResetVT()
	case resetter:
		r.Reset()
	default:
		return fmt.Errorf("vtcodec: %T has UnmarshalVT but neither ResetVT nor Reset; "+
			"it cannot be reused safely because UnmarshalVT is merge-shaped", m)
	}
	return vu.UnmarshalVT(src)
}

// Name returns the gRPC content-subtype for protobuf.
func (Codec) Name() string { return "proto" }
