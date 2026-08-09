package pgrpc

import "context"

// Unary performs a unary RPC: marshal in, send, receive, unmarshal into out.
//
// It is the resolved-config entry point. A reusable caller resolves cfg once,
// outside its request loop, and calls this; the ergonomic path goes through
// UnaryOpts, which resolves a variadic option list per call and delegates here.
//
// There is deliberately NO codec parameter. The codec is resolved from the two
// channels that exist — cfg.Codec, else the Client's — by Client.CodecFor, the
// single resolution rule. A parameter would make WithCallCodec dead: the body
// would use the parameter and silently ignore cfg.Codec, so a user A/B-ing a
// vtprotobuf codec against the reflection one would get the client codec's wire
// encoding with no diagnostic anywhere.
//
// scratch is the request-side buffer and respScratch the response-side one.
// Either may be nil, which means "allocate a fresh one for this call" — the
// honest cost of the ergonomic path, and the reason that path has no
// shared-buffer hazard. A reusable caller passes its own and gets reuse across
// calls.
//
// A package-level sync.Pool is deliberately not used for the nil case. A buffer
// could only go back after the response was unmarshalled, and never on a path
// where the codec retained bytes out of it — a condition this function cannot
// check.
//
// out is reset by the codec before the response is applied, so the same out
// message can be handed to every iteration of a loop.
//
// method is "/package.Service/Method". poseidon validates only the leading
// slash; everything after it reaches the wire verbatim.
//
// Errors:
//   - a poseidon *Status is returned UNCHANGED, so errors.As(err, &st) behaves
//     exactly as it would against the connection directly. Error has a POINTER
//     receiver, so the target must be declared as var st *pgrpc.Status.
//   - a codec failure is a *CodecError, which unwraps to BOTH a *Status with
//     code Internal and the codec's own error.
//   - every other poseidon or transport error passes through verbatim. Those
//     are not *Status values — see StatusOf.
func Unary(ctx context.Context, c *Client, cfg *CallConfig,
	method string, in, out any, scratch, respScratch *[]byte) error {
	if cfg.Err != nil {
		return cfg.Err
	}
	cd := c.CodecFor(cfg)
	if scratch == nil {
		var b []byte
		scratch = &b
	}
	if respScratch == nil {
		var b []byte
		respScratch = &b
	}

	// Marshal before touching the transport. A request that cannot be encoded
	// must not open a stream: a half-opened stream on a marshal bug leaves the
	// server waiting out the whole deadline for a request that is never coming.
	req, err := cd.MarshalAppend((*scratch)[:0], in)
	if err != nil {
		// Keep the buffer even here. MarshalAppend's contract says the returned
		// slice is unspecified on error, but its ARRAY is still the caller's,
		// and a looping caller that lost it would reallocate on every failure.
		*scratch = req[:0]
		return NewCodecError(OpMarshal, cd.Name(), in, err)
	}
	*scratch = req // keep the grown array

	raw, err := unaryRaw(ctx, c.Invoker(), method, req, (*respScratch)[:0], cfg.MD, cfg.Pass)
	// InvokeInto returns dst[:0] rather than nil on error, precisely so that a
	// looping caller keeps its buffer. Storing the result on BOTH paths is what
	// preserves that property here.
	*respScratch = raw
	if err != nil {
		return err // *Status or transport error, unchanged
	}

	if err := cd.Unmarshal(raw, out); err != nil {
		return NewCodecError(OpUnmarshal, cd.Name(), out, err)
	}
	return nil
}

// UnaryOpts is the ergonomic unary entry point. It resolves opts against the
// Client's defaults and delegates to Unary with no caller-supplied buffers.
//
// It costs one CallConfig allocation per RPC even with zero options, because
// &cfg escapes through the interface call in Apply. That is precisely the
// allocation the resolved-config path above exists to avoid, and it is a named
// line item in the allocation budget rather than a footnote.
func UnaryOpts(ctx context.Context, c *Client, method string, in, out any,
	opts ...CallOption) error {
	var cfg CallConfig
	if err := c.resolve(&cfg, opts); err != nil {
		return err
	}
	return Unary(ctx, c, &cfg, method, in, out, nil, nil)
}
