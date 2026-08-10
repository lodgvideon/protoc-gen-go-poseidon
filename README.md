# protoc-gen-go-poseidon

A `protoc` plugin that generates typed gRPC **clients** on
[poseidon-http-client](https://github.com/lodgvideon/poseidon-http-client)'s
from-scratch HTTP/2 stack — what `protoc-gen-go-grpc` does for grpc-go, without
linking grpc-go at all.

> **Status: working, unreleased.** Every call shape round-trips against a real
> grpc-go server under `-race`, and CI builds five modules on every push. There
> is no tag yet and the generated API is not frozen. See
> [What is not done](#what-is-not-done).

## Why

poseidon's `grpc` package is a transport, not a framework: messages cross its
API as `[]byte`, and there is no protobuf dependency and no code generation.
That is the right shape for a library aimed at load generators, but it leaves
every caller hand-writing the method path and the marshal calls:

```go
resp, err := cc.Invoke(ctx, "/helloworld.Greeter/SayHello", reqBytes, nil)
```

This plugin closes that gap without pushing protobuf down into poseidon.

## Install

```bash
go install github.com/lodgvideon/protoc-gen-go-poseidon/cmd/protoc-gen-go-poseidon@latest
```

`buf.gen.yaml`:

```yaml
version: v2
plugins:
  - local: protoc-gen-go
    out: gen
    opt: paths=source_relative
  - local: protoc-gen-go-poseidon
    out: gen
    opt: paths=source_relative
```

Output lands in a **sub-package** of the message package —
`gen/helloworld/poseidon/` — which is what makes collisions with
`protoc-gen-go` and `protoc-gen-go-grpc` impossible rather than unlikely. See
[Naming](#naming).

## Use

```go
cc, err := grpc.Dial(ctx, "api.example.com:443", grpc.Options{
    Conn: conn.ConnOptions{
        Dialer: &conn.TLSDialer{Config: &tls.Config{NextProtos: []string{"h2"}}},
    },
})
if err != nil {
    return err
}
defer cc.Close()

client := poseidon.NewGreeterClientOn(cc)

reply, err := client.SayHello(ctx, &helloworld.HelloRequest{Name: "world"})
```

Streaming reads as you would expect, and the server-streaming iterator closes
the stream for you — on a normal end, on `break`, on `return` and on a panic:

```go
stream, err := client.LotsOfReplies(ctx, req)
if err != nil {
    return err
}
for reply, err := range stream.All(ctx) {
    if err != nil {
        return err
    }
    use(reply)
}
```

### The two faces

Every service generates two, because a load generator wants something an
ordinary caller does not.

**`GreeterClient`** is ergonomic. It allocates a response per call and takes
options per call. Use it for anything that is not a hot loop.

**`GreeterCaller`** reuses buffers. It owns the request scratch, the response
scratch and the resolved call configuration, so a steady loop allocates nothing
in this layer — **0 B/op, 0 allocs/op**, measured, against the ergonomic face's
112 B in 3. See [docs/ALLOCATIONS.md](docs/ALLOCATIONS.md). One Caller serves one goroutine and one in-flight RPC; a
concurrent second call returns `pgrpc.ErrCallerInUse` rather than corrupting a
request body.

```go
x := poseidon.NewGreeterCallerOn(cc)
x.Config().Apply(pgrpc.WithMetadata(md)) // resolved ONCE, outside the loop

in, out := &helloworld.HelloRequest{Name: "world"}, &helloworld.HelloReply{}
for {
    if err := x.SayHello(ctx, in, out); err != nil {
        return err
    }
    use(out) // reset by the codec on every call
}
```

### Codecs

The codec seam is append-shaped, so it fits both `google.golang.org/protobuf`
and [vtprotobuf](https://github.com/planetscale/vtprotobuf):

```go
pgrpc.NewClient(cc, pgrpc.WithCodec(vtcodec.Codec{}))
```

`vtcodec` finds vtprotobuf's methods by interface probe, so it adds no module
dependency, and falls back per message for a partly-generated schema. Three
traps in that path are handled explicitly and each has a test — see the doc
comments on `pgrpc/vtcodec`.

## Naming

Generated code goes into its own Go package, so nothing it declares can collide
with `*.pb.go` or `*_grpc.pb.go`: distinct package blocks are disjoint scopes.

That is deliberate rather than convenient. The alternative — a "magic infix" in
the shared package — cannot be proven safe: `GoCamelCase` drops `_` only before
a lowercase letter, so for **any** identifier a plugin might pick there is a
`.proto` symbol that produces exactly it. `separate_package=false` is therefore
rejected rather than shipped half-proven.

Two service shapes are refused outright:

- two methods whose Go names coincide (`SayHello` and `say_hello`), which would
  be a redeclaration;
- a method named `Enter` or `Leave`, which would shadow `pgrpc.Guard`'s promoted
  method on the Caller and turn the generated body's own `x.Enter()` into
  infinite recursion — the only collision here that the compiler cannot catch.

## Options

Passed as `--go-poseidon_opt=key=value`, or bare for a boolean.

| Option | Default | Effect |
|---|---|---|
| `package_suffix` | `poseidon` | sub-package name and directory |
| `callers` | `true` | emit the buffer-reusing `XCaller` face |
| `interfaces` | `true` | emit the `XClient` interface |
| `method_consts` | `true` | emit the full-method-name constants |
| `runtime_import` | the `pgrpc` path | redirect every runtime reference, for a fork or a vendor path |
| `default_codec` | `protocodec` | emit the `NewXClientOn`/`NewXCallerOn` constructors that supply a codec |
| `separate_package` | `true` | the only supported mode; `false` is rejected |

`paths=`, `module=`, `M<file>=` and `annotate_code` are protogen's and work as
they do for every other Go plugin.

## Layout

Five modules, because each proves something the others cannot:

| Module | Purpose |
|---|---|
| `.` | the plugin binary. Requires **only** `google.golang.org/protobuf` |
| `pgrpc/` | the runtime generated code calls into. Requires poseidon and protobuf |
| `testdata/` | compiles the checked-in generated output against `pgrpc` and **nothing else** |
| `test/e2e/` | drives that output against a real grpc-go server |
| `examples/loadgen/` | the only module shaped like a **consumer**; its dependency graph is CI's proof that a user links the runtime, not the plugin, and never grpc-go |

`testdata` is a module rather than a directory because Go's toolchain skips any
directory named `testdata` — a module rooted there is what makes those files
buildable at all.

## What is tested

- Golden files over seven option combinations, including a bare boolean key.
- Nine negative cases: the `Enter` shadow, colliding method Go names, the flat
  mode, a keyword `package_suffix`, an invalid identifier, an empty one, an
  unknown codec, an empty runtime import, and an unknown option.
- The checked-in fixture is compared against the golden, so the compile proof
  cannot pass on a stale file.
- All four call shapes against grpc-go over a loopback socket, under `-race`,
  including a bidirectional call driven by two goroutines.
- Trailers-Only and error-after-headers, which poseidon derives through
  different code paths.

## What is not done

- **No release tag.** The generated API may still change.
- **The client's share of a streaming call is not separable.** A stream needs a
  real server in-process, so only the difference between paired arms is
  attributable; see [docs/ALLOCATIONS.md](docs/ALLOCATIONS.md).
- **No BSR remote plugin.** Local binary only.
- **Flat mode** (`separate_package=false`) is deferred; it needs a mechanical
  gate over everything the two upstream generators can emit.
- **`docs/` is one file.** Only the allocation figures are written up; the rest of the reasoning lives in doc comments.

## Upstream

Some allocation work belongs in poseidon and is tracked there. None of it
blocks this plugin — generated code gets faster as these land, with no
regeneration:

- [#437](https://github.com/lodgvideon/poseidon-http-client/issues/437) — tracker
- [#460](https://github.com/lodgvideon/poseidon-http-client/issues/460) —
  `callOptions` escapes to the heap even with zero options
- [#461](https://github.com/lodgvideon/poseidon-http-client/issues/461) —
  headers and trailers cloned unconditionally, `Invoke` reads neither
- [#462](https://github.com/lodgvideon/poseidon-http-client/issues/462) —
  the content-type subtype cannot reach the wire, so `Codec.Name` is
  diagnostics-only
- [#463](https://github.com/lodgvideon/poseidon-http-client/issues/463) —
  no escape hatch for the `grpc-` namespace, so `grpc-trace-bin` cannot be sent

## License

MIT. See [LICENSE](LICENSE).
