# Generated code

What the plugin emits, and why each piece is shaped the way it is. The complete
output for a four-shape service is checked in at
[`testdata/golden/default.golden`](../testdata/golden/default.golden) and
compiled by CI, so this document can be wrong and the golden cannot.

## Where it goes

`gen/helloworld/helloworld.proto` produces
`gen/helloworld/poseidon/helloworld_poseidon.pb.go`, in package `poseidon`.

A **sub-package**, not the message package, and that is the whole collision
argument: distinct Go package blocks are disjoint scopes, so nothing emitted
here can collide with `protoc-gen-go`'s or `protoc-gen-go-grpc`'s output. The
sub-package imports the message package; never the reverse.

Identifiers emitted by *this* plugin are tracked per **output directory**, so
several `.proto` files routing into one Go package cannot silently emit the same
name. That check has an honest limit worth knowing: it covers **one plugin
invocation**. `buf`'s default strategy runs the plugin once per source
directory, so two colliding files in different source directories are never
seen together and the check cannot fire; `protoc` passes the whole set at once,
where it can. Nothing inside a plugin can widen that — the request is all it
gets.

The alternative — a "magic infix" in the shared package — is not provable.
`GoCamelCase` drops `_` only before a lowercase letter, so for any identifier a
plugin might reserve there is a `.proto` symbol that produces exactly it.
`separate_package=false` is rejected rather than shipped half-proven.

## What is emitted, per service

For `service Greeter` with `SayHello` (unary), `LotsOfReplies` (server),
`LotsOfGreetings` (client) and `BidiHello` (bidi):

| Identifier | Kind |
|---|---|
| `const _ = pgrpc.SupportPackageIsVersion1` | version guard |
| `Greeter_SayHello_FullMethodName`, … | wire path per method |
| `Greeter_LotsOfRepliesClient`, … | pointer alias per streaming method |
| `GreeterClient` | interface |
| `greeterClient` | implementation |
| `NewGreeterClient(*pgrpc.Client)` | constructor |
| `NewGreeterClientOn(pgrpc.Invoker, ...ClientOption)` | constructor that supplies the codec |
| `GreeterCaller` | the buffer-reusing face |
| `NewGreeterCaller`, `NewGreeterCallerOn` | its constructors |

### The version guard

```go
const _ = pgrpc.SupportPackageIsVersion1
```

If a future plugin emits code this runtime cannot support, that release drops
the constant and introduces `…IsVersion2`. A stale runtime then fails with
`undefined: pgrpc.SupportPackageIsVersion2`, which names the actual problem,
rather than failing somewhere inside a generated call with a mismatched
signature.

### Method-name constants

Built from the **descriptor** names, never the Go names:

```go
Greeter_SayHello_FullMethodName = "/helloworld.Greeter/SayHello"
```

The two differ for anything containing an underscore, and this string is what
reaches the server.

### Stream aliases are pointers, always

```go
type Greeter_BidiHelloClient = *pgrpc.BidiStream[helloworld.HelloRequest, helloworld.HelloReply]
```

The `*` is not cosmetic. The stream types carry a mutex and three atomics and
must never be copied. A value alias would let `var s Greeter_BidiHelloClient`
compile and would turn passing one by value into a `go vet` copylocks finding in
**your** build, from a declaration you did not write.

The type arguments are the value types, because `Recv(ctx, out *Resp)` and
`RecvNew() (*Resp, error)` both work in terms of `*Resp`.

## The two faces

### `GreeterClient` — ergonomic

```go
func (x *greeterClient) SayHello(ctx context.Context, in *helloworld.HelloRequest,
	opts ...pgrpc.CallOption) (*helloworld.HelloReply, error) {
	out := new(helloworld.HelloReply)
	if err := pgrpc.UnaryOpts(ctx, x.c, Greeter_SayHello_FullMethodName, in, out, opts...); err != nil {
		return nil, err
	}
	return out, nil
}
```

Allocates a response per call and resolves options per call: 3 allocations and
112 bytes in this layer, decomposed in [ALLOCATIONS.md](ALLOCATIONS.md). Use it
for anything that is not a hot loop.

### `GreeterCaller` — buffer-reusing

```go
func (x *GreeterCaller) SayHello(ctx context.Context, in *helloworld.HelloRequest,
	out *helloworld.HelloReply) error {
	if err := x.guard.Enter(); err != nil {
		return err
	}
	defer x.guard.Leave()
	return pgrpc.Unary(ctx, x.c, &x.cfg, Greeter_SayHello_FullMethodName, in, out, &x.buf, &x.respBuf)
}
```

Zero allocations in this layer. It owns the request scratch, the response
scratch and the resolved configuration, which is why it serves **one goroutine
and one in-flight RPC** — a second concurrent call gets `ErrCallerInUse` rather
than a corrupted request body.

The `Guard` comes from `pgrpc` rather than being written into the generated
struct, so a generated file imports exactly three packages — `context`, `pgrpc`
and its own message package — and never `sync/atomic`.

It is a **named field**, `guard`, not an embedded one. Embedding was the
original shape and it was wrong in a way a tag would have made permanent:
`Enter` and `Leave` became exported methods on every generated type, and every
method ever added to `pgrpc.Guard` would have retroactively forbidden a `.proto`
method name for every user of this plugin — a coupling written down nowhere. A
named field costs one selector and reserves nothing.

### Why the streaming Caller methods exist at all

They save one resolved configuration per **stream**, not per message. That is
worth little, and they are emitted anyway so a Caller is a complete face — a
virtual user should not need two objects. The saving is stated plainly rather
than implied, so nobody mistakes them for a per-message lever.

## Call shapes

| Shape | Client-face signature |
|---|---|
| unary | `(ctx, in, opts...) (*Resp, error)` |
| server-streaming | `(ctx, in, opts...) (*ServerStream[Resp], error)` |
| client-streaming | `(ctx, opts...) (*ClientStream[Req, Resp], error)` |
| bidirectional | `(ctx, opts...) (*BidiStream[Req, Resp], error)` |

Client-streaming and bidirectional take no request: theirs go through the
stream.

**`ClientStream` has no `Recv`.** poseidon permits concurrent send and receive —
that is what makes bidi work — but the receive side must be driven by one
goroutine. On a client-streaming call the natural mistake is a `Recv` loop in
one goroutine while another sends, then `CloseAndRecv` from the sender: two
goroutines inside poseidon's unguarded receive state. Leaving the method off the
type makes that a compile error. Use `SendLastAndRecv` or `CloseAndRecv`.

**`ServerStream.All` closes the stream; `BidiStream.All` does not.** On a
server-streaming call the iterator's goroutine owns everything, so closing on
`break`, `return`, error or panic is leak-proofing with no ambiguity. On a bidi
call the sending goroutine's lifetime is not the iterator's, and a `break`
silently cancelling another goroutine's in-flight `Send` would be surprising.

**`Header` behaves differently per shape, and has to.** On a server-streaming
stream it is populated eagerly by the constructor and is a pure cache read. On
client-streaming and bidi it cannot be: a server sends response headers when it
starts answering, which on those shapes is after the client has sent something,
and a constructor blocking for them has not let you send anything yet. There it
returns `ErrHeaderNotReady` until the first successful receive.

**A nil error from `Header` does not mean the call succeeded.** On the
Trailers-Only shape one HEADERS frame carries both the response headers and
`grpc-status`, so `Header` returns a block and no error for a call that already
failed. Classify with the error from a receive, never with `Header`'s.

## Options

Everything is on by default. See the table in the [README](../README.md).

The two worth explaining:

**`runtime_import`** redirects every `pgrpc` reference, for a fork or a vendored
path. Generated code names no import path literally; every reference goes
through protogen's ident machinery, so this is a one-line change with no risk of
a half-rewritten file.

**`default_codec=none`** drops the `…On` constructors. Take it if you are a
vtprotobuf-only user who does not want the reflection codec linked — though note
your message package already links the protobuf runtime, so the saving is
smaller than it looks.

## What the generator refuses

Failing generation, never renaming. Generated identifiers are the package's
public API; a silent `_` suffix would break every call site in your repository
from a diff that mentions only an unrelated new message, and it would not even
be deterministic across invocations.

- a method whose Go name is `Enter`, `Leave` or `Config` — see above. All three
  are reported in one pass, so a service with several needs one fix round, not
  one per method;
- two methods whose Go names coincide, which proto's own uniqueness rule permits
  (`SayHello` and `say_hello`);
- `separate_package=false`, a `package_suffix` that is a keyword or not an
  identifier, an unknown `default_codec`, an empty `runtime_import`, and any
  option name the plugin does not know.

Every one of those has a fixture and a test.

One more refusal exists in the code and **cannot be reached from a valid
`.proto`**: a service whose Go name begins with a rune that has no lower case,
which would make the unexported implementation struct share the exported
interface's name. protoc rejects non-ASCII identifiers outright — verified, it
says so — and `GoCamelCase` uppercases the first letter of an ASCII name, so the
case cannot arise. The branch stays because it guards an invariant the generator
relies on, and it is tested at the helper rather than through a descriptor,
which is the honest place for a check nothing can trigger.
