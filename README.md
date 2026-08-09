# protoc-gen-go-poseidon

A `protoc` plugin that generates typed gRPC **clients** on top of
[poseidon-http-client](https://github.com/lodgvideon/poseidon-http-client)'s
`grpc` package — the way `protoc-gen-go-grpc` does for grpc-go, but emitting
code that never links grpc-go at all.

> **Status: pre-alpha, under construction.** Nothing is released, the generated
> API is not stable, and there is no compatibility promise yet. See
> [Milestones](#milestones).

## Why

poseidon's `grpc` package is a transport, not a framework: messages cross its
API as `[]byte`, there is no protobuf dependency and no service registry. That
is the right shape for a library aimed at load generators — a pre-marshaled
fixture replayed millions of times costs nothing to re-encode — but it means
every caller hand-writes the method path and the marshal/unmarshal calls:

```go
resp, err := cc.Invoke(ctx, "/helloworld.Greeter/SayHello", reqBytes, nil)
```

This plugin closes that gap without pushing protobuf down into poseidon. Two
properties drive the design:

- **poseidon's root module keeps its four dependencies.** The plugin binary and
  the runtime that generated code calls into both live here, so
  `google.golang.org/protobuf` never enters poseidon's dependency graph.
- **The generated client has a buffer-reusing path, not only an ergonomic one.**
  The target user is a load generator, so allocations per RPC are a first-class
  concern rather than an afterthought. The codec seam is append-shaped
  (`MarshalAppend`) so it fits both `google.golang.org/protobuf` and
  [vtprotobuf](https://github.com/planetscale/vtprotobuf); hard-wiring
  `proto.Marshal` would forfeit the premise.

## What it is not

Name resolution, load balancing, retry policy and per-call authentication live
above poseidon's `grpc` package and stay there. This plugin does not change
that, and does not implement grpc-go's `ClientConnInterface` — doing so would
drag the entire grpc-go runtime back into the build, which is the thing being
avoided.

It generates **clients only**. poseidon is a client library.

## Install

Not yet. There is no tagged release.

## Milestones

**v0.1.0**

- unary and server-streaming methods
- ergonomic and buffer-reusing call forms
- pluggable append-shaped codec, with a `google.golang.org/protobuf` default
- golden-file tests over a checked-in `FileDescriptorSet`, so the generator is
  testable without `protoc` on `PATH`
- an end-to-end test against a real gRPC server

**Deferred to v0.2.0**

- client-streaming and bidirectional methods
- a vtprotobuf codec shipped in-tree
- publication as a remote `buf` plugin

## Upstream

Some of the allocation work has to happen in poseidon itself. It is tracked
there, and none of it blocks this plugin — the generated code works against
poseidon's API as it stands today and gets faster when these land, without
regeneration:

- [#437](https://github.com/lodgvideon/poseidon-http-client/issues/437) —
  tracking issue
- [#434](https://github.com/lodgvideon/poseidon-http-client/issues/434) /
  [#435](https://github.com/lodgvideon/poseidon-http-client/issues/435) —
  buffer-reusing receive and unary paths
- [#436](https://github.com/lodgvideon/poseidon-http-client/issues/436) —
  pooling the per-RPC `grpc.Stream`

## License

MIT. See [LICENSE](LICENSE).
