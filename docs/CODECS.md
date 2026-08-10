# Codecs

The seam between a message and its bytes. It exists so that poseidon never has
to know what protobuf is, and so that a user who has paid for vtprotobuf can
actually collect on it.

```go
type Codec interface {
	MarshalAppend(dst []byte, m any) ([]byte, error)
	Unmarshal(src []byte, m any) error
	Name() string
}
```

## Why it is append-shaped

`Marshal(m) ([]byte, error)` would allocate a buffer per call and hand ownership
to the caller, which is the allocation the whole buffer-reusing path exists to
remove. `MarshalAppend` writes into memory the caller already owns, so a steady
loop marshals into the same array forever.

It also happens to be the shape both real implementations already have:
`proto.MarshalOptions.MarshalAppend` and vtprotobuf's
`MarshalToSizedBufferVT`. A `Marshal`-shaped interface would have forced an
adapter allocation onto both.

## Why `any` and not `proto.Message`

Because `pgrpc` must not import protobuf. That is not squeamishness: poseidon's
whole point is that a caller who does not want protobuf does not link it, and a
`proto.Message` in this interface would drag the reflection runtime into every
build.

It also keeps the door open for a codec over messages that are not
`proto.Message` v2 at all — gogo, hand-rolled, or a fixture type that only knows
how to append its own bytes.

The cost is that a type error surfaces at run time rather than compile time. It
surfaces as a `*CodecError`, which carries the message's Go type and unwraps to
both a `Status` with code `Internal` and the codec's own error.

## `Unmarshal` is reset-shaped, and that is a contract

`Unmarshal(src, m)` must **discard m's previous contents**. Every caller in this
module reuses the destination message across calls — that is the point of the
`XCaller` face and of `Recv(ctx, out)` — so a merge-shaped implementation makes
each response accumulate the previous one's repeated fields.

That bug appears only under reuse, which is precisely the load-generator path,
and never in a test that unmarshals into a fresh message.

## `protocodec` — the default

`google.golang.org/protobuf`. Nothing surprising: `MarshalOptions.MarshalAppend`
and `UnmarshalOptions` with `Merge` left false, which is the reset shape.

It is what the generated `NewXClientOn` and `NewXCallerOn` constructors supply,
which is why forgetting to configure a codec is not a common mistake. Using
`NewXClient` with a hand-built `pgrpc.Client` and no codec panics at
construction, deliberately: a nil codec would otherwise surface as a nil
dereference on the first RPC, arbitrarily far from its cause.

## `vtcodec` — vtprotobuf where available

```go
pgrpc.NewClient(cc, pgrpc.WithCodec(vtcodec.Codec{}))
```

The zero value works and falls back to `protocodec` for any message vtprotobuf
did not generate for, so a partly-generated schema is fine.

**It does not import vtprotobuf.** The generated methods are found structurally,
by interface probe, so this package adds no module dependency and a user without
vtprotobuf pays nothing.

### What the probes have to get right

Each of these is a way to be silently wrong — not to fail, to be wrong — which
is why they are probed the way they are and why each has a test.

**`MarshalToSizedBufferVT` fills backwards.** It writes from the end of the
slice towards the front, so the slice handed to it must be **exactly** `SizeVT()`
bytes long, not merely large enough. A slice that is too long truncates the
message from the front and produces bytes that still parse.

**The `strict` feature renames the method.** vtprotobuf's `strict` option emits
`MarshalToSizedBufferVTStrict`. Probing only the plain name would send every
strict-generated schema down the fallback path — correct output, none of the
speed, no diagnostic.

**`MarshalVT` is the wrong entry point.** It allocates its own buffer, which is
the exact allocation the append shape exists to remove. The size-then-fill pair
is preferred wherever it exists.

**`UnmarshalVT` is merge-shaped.** It is gogo-derived: it appends to repeated
fields and leaves absent scalars alone. Since `Codec.Unmarshal` is contractually
reset-shaped, a reset must run first.

**`ResetVT` usually does not exist.** It comes from vtprotobuf's `pool` feature
only, and only for messages opted in with `option (vtproto.mempool) = true`. On
a canonical `features=marshal+unmarshal+size` schema there is no `ResetVT` at
all — so folding it into the unmarshal probe would make the fast path silently
never match. The probes are separate: `ResetVT` if present, otherwise `Reset`,
which protoc-gen-go emits for every message.

**`UnmarshalVTUnsafe` is deliberately not probed.** It aliases the input buffer
into the message, and this package hands it the receive scratch, which the next
receive overwrites.

### One more thing that is not a probe

`fallback()` returns a package-level boxed `protocodec.Codec{}` rather than
constructing one per call. Returning the struct directly would convert a value
to an interface at every fallback and escape — one allocation on the path a
partly-generated schema takes for **every** message vtprotobuf skipped.

## `Name` is diagnostics-only, today

`Name` returns the gRPC content-subtype — `"proto"` — and **it does not reach
the wire.** poseidon hard-wires `content-type: application/grpc` and treats the
header as reserved, so a subtype cannot be sent. That is tracked upstream as
[#462](https://github.com/lodgvideon/poseidon-http-client/issues/462).

Until it lands, `Name` is what appears in a `CodecError` and nothing else. The
asymmetry is real and worth knowing: poseidon's receive side already *accepts* a
subtype from a server, so the client accepts what it cannot say.

## Writing your own

Three methods, and the two rules above: append, and reset. A JSON codec, a
fixture codec that returns pre-marshalled bytes, or a codec that counts calls
for a test are all a few lines.

```go
type fixtureCodec struct{ wire []byte }

func (c fixtureCodec) MarshalAppend(dst []byte, _ any) ([]byte, error) {
	return append(dst, c.wire...), nil
}
func (c fixtureCodec) Unmarshal([]byte, any) error { return nil }
func (fixtureCodec) Name() string                  { return "fixture" }
```

That one is not a toy: replaying a pre-marshalled fixture is the case the whole
buffer-reusing path is built for, and it is what the `_Nop` arm of every
benchmark in [ALLOCATIONS.md](ALLOCATIONS.md) uses to separate this module's
cost from protobuf's.

Report failures with `pgrpc.NewCodecError`, not a bare error. The zero value of
`CodecError` carries a zero `Status`, whose code is `OK` — the exact failure mode
the type exists to prevent.

## Per-call override

```go
resp, err := client.SayHello(ctx, req, pgrpc.WithCallCodec(vtcodec.Codec{}))
```

Resolution happens in exactly one place, `Client.CodecFor`: a non-nil per-call
codec wins, nil inherits the client's. Nothing else resolves a codec, because
two channels where only one is read is how a per-call codec silently produces
the wrong wire encoding.
