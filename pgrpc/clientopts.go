package pgrpc

// ClientOption configures a Client at construction.
type ClientOption func(*Client)

// WithCodec sets the codec every call on this client uses. It is mandatory:
// NewClient panics if no codec was supplied. See NewClient for why that is a
// panic rather than an error.
func WithCodec(cd Codec) ClientOption {
	return func(c *Client) { c.codec = cd }
}

// WithDefaultCallOptions prepends options to every call on this client.
//
// Per-call options are applied AFTER these, so a per-call scalar setting wins,
// and per-call metadata is APPENDED to the default metadata rather than
// replacing it.
//
// ALIASING. Default metadata must never be appended to in place: if the default
// slice has spare capacity, two concurrent RPCs appending their own header
// write the same index. That is not merely a race-detector finding —
// poseidon's buildHeaders reads md[i] synchronously inside NewStream, so one
// virtual user's header, routinely a credential, ships on another user's RPC,
// and without Indexing: IndexNever it also enters the connection's shared HPACK
// dynamic table.
//
// Nothing special is needed here to prevent that, because CallConfig tracks
// ownership: metadata arriving from an option is recorded as borrowed, and the
// first path that appends copies it with a full-slice expression first. The
// design called for additionally storing the default clipped, as a second
// barrier against an append landing in shared memory; that is redundant with
// the ownership flag and is deliberately not done, because a second mechanism
// guarding the same invariant is a second thing that can drift out of step with
// it.
func WithDefaultCallOptions(opts ...CallOption) ClientOption {
	return func(c *Client) { c.defOpts = append(c.defOpts, opts...) }
}
