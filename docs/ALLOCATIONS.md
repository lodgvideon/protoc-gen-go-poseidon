# Allocations

Every number here is read off a benchmark in `pgrpc/bench_test.go`. None is
reasoned about, and none is a target — they are what the code does today, on the
machine and Go version named at the bottom.

Reproduce:

```bash
cd pgrpc && go test -run='^$' -bench=. -benchmem -count=1 .
```

## Why there is no socket in these benchmarks

A benchmark that dials an in-process gRPC server charges that server's
allocations to the same counter. The result answers "what does this layer plus a
server cost", which nobody can act on: you cannot tell your own regression from
the server's.

So the transport here is a fake that does no I/O, and what is measured is
exactly the code between a caller and poseidon's `Invoke`. The end-to-end suite
in `test/e2e` proves behaviour; it deliberately publishes no allocation figures.

## Why every row is run twice

Each shape runs once with a codec that allocates nothing and once with the real
protobuf codec. The first column is what this package costs. The second includes
what protobuf costs on top, which no amount of work here can remove.

The zero-allocation codec is not a strawman. A load generator replaying
pre-marshalled fixtures — the case this module is built for — genuinely has one.

## Unary

| Path | Codec | ns/op | B/op | allocs/op |
|---|---|---:|---:|---:|
| `Unary`, caller buffers, resolved config | nop | 13.9 | 0 | **0** |
| `Unary`, caller buffers, resolved config | proto | 111.5 | 4 | 1 |
| `Unary`, nil buffers, resolved config | nop | 33.5 | 16 | 2 |
| `UnaryOpts` (ergonomic) | nop | 61.6 | 112 | 3 |
| `UnaryOpts` (ergonomic) | proto | 173.7 | 120 | 4 |

**The first row is the claim this module is built around**, and it holds: with a
resolved configuration and caller-owned buffers, this layer adds nothing.

**The ergonomic path's three allocations decompose exactly**, which matters more
than the total, because it says which are avoidable:

| | allocs | B | what |
|---|---:|---:|---|
| config | 1 | 96 | the `CallConfig`. `&cfg` escapes through the interface call in `Apply`; `go build -gcflags=-m` prints `moved to heap: cfg` |
| buffers | 2 | 16 | request and response, allocated fresh because a nil scratch means exactly that |

Both are avoidable, and the generated `XCaller` face avoids both: it owns a
resolved config and both buffers.

**Protobuf costs one allocation**, for the string the response unmarshals into.
It does not scale with the number of options or the shape of the call.

## Metadata

| Path | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `pgrpc.Metadata`, steady state | 36.6 | 0 | **0** |
| `grpc.AppendMetadata` through `WithHeader` | 227.3 | 404 | 8 |

Two entries, one text and one `-bin`, values changing every iteration and keys
not. That is the only case the builder claims to win, and it is the case a load
generator actually has: a rotating request id or token against a fixed key set.

The comparison row exists so the claim can be falsified. Without it "allocation-
free in the steady state" is unfalsifiable — the builder would look good against
nothing.

`grpc.AppendMetadata` lowercases the key and converts it to bytes on **every**
call; the builder interns the lowercased name on first use and keeps it across
`Reset`, and encodes `-bin` values into a reused arena.

**It is the wrong tool for metadata that does not change.** Constant metadata
should be built once with `grpc.AppendMetadata` and passed through
`WithMetadata` forever; the builder buys nothing there and costs a type.

## Option resolution

| Path | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `CallConfig.Apply(WithMetadata(md))` | 48.0 | 120 | 2 |

One allocation is the escaping `CallConfig`, the other is the option value
itself: a struct carrying a slice header does not fit in an interface word, so
every `WithMetadata(md)` is boxed. This is why the documented allocation-free
form is assigning `CallConfig.MD` once, outside the request loop, rather than
passing an option per call.

## What is not measured yet

- **Streaming.** Per-message send and receive costs are not benchmarked. The
  buffers exist (`sendSide.sendBuf`, `baseStream.recvBuf`) and are reused, but
  there is no figure, so this document does not claim one.
- **Throughput.** Nothing here is a ns/op claim about real RPCs; the fake
  transport returns instantly, so the ns/op column measures this layer's CPU
  cost and nothing else.
- **The upstream floor.** poseidon still allocates on its own unary path —
  `callOptions` escapes even with zero options, and response headers and
  trailers are cloned unconditionally on a call that reads neither. Those are
  tracked upstream and are invisible to these benchmarks, which stop at
  `Invoke`.

## Environment

Go 1.26.4, windows/amd64, 16 logical CPUs. Figures move between machines; the
decompositions and the zeros do not.
