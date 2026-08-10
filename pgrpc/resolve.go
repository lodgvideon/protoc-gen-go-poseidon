package pgrpc

// resolve is THE option-resolution procedure. Every variadic entry point in
// this package goes through it and nothing else reimplements it.
//
// The order is load-bearing in two ways. The Client's defaults are applied
// first so that a per-call scalar setting wins and per-call metadata is
// appended to the default rather than replacing it. And every option is applied
// before the transport is touched, so that cfg.Err — which latches the first
// failure, because options are values applied in a loop and cannot return
// errors — is available to short-circuit the call.
//
// Silently dropping a malformed header would send the RPC without the
// credential the caller thought they had attached, which is why the error is
// latched rather than ignored, and why every caller of this function must check
// cfg.Err before doing anything else.
//
// The default metadata needs no special copying here. CallConfig records
// metadata arriving from an option as borrowed, so the first per-call append
// materialises it with a full-slice expression — which is what stops one
// virtual user's header from landing in the shared default array that another
// user's in-flight RPC is reading.
func (c *Client) resolve(cfg *CallConfig, opts []CallOption) error {
	cfg.Apply(c.defOpts...)
	cfg.Apply(opts...)
	return cfg.Err()
}
