# loadgen

A small load generator over a generated poseidon client, and the repository's
proof of what a **user's** binary links.

```bash
go run . -addr 127.0.0.1:50051 -plaintext -users 32 -conns 2 -duration 5s
```

```
requests   224250 in 4.999s (44862/s)
latency    p50 547µs  p90 1.105ms  p99 2.087ms  max 11.144ms
outcomes
  OK                     224218  100.0%
  DEADLINE_EXCEEDED          32    0.0%
```

Real output, against a grpc-go `Greeter` on loopback. Read it as a demonstration
of the client's shape, not as a benchmark: both ends were on one machine, so the
latency figures are a scheduler measurement more than a network one.

**Those 32 deadline-exceeded are the interesting line.** There are exactly 32,
one per virtual user: the call each was in the middle of when the run's own
deadline expired. Nothing failed. They are here because the report buckets with
`pgrpc.StatusOf` — code that reached for `errors.As(err, &st)` instead would
have found no `*Status` on a cancelled call, taken the zero value, and filed all
32 under **OK**.

That is the entire reason `StatusOf` exists, and it is why a load generator must
not classify outcomes any other way: poseidon returns a status for anything that
reached the server and a bare transport error for anything that did not.

## The three rules it demonstrates

1. **One Caller per virtual user**, built once outside the request loop. A
   Caller owns its request scratch, its response scratch and its resolved call
   configuration. Sharing one across goroutines is what `pgrpc.ErrCallerInUse`
   reports rather than silently corrupting a request body.
2. **Reuse the messages too.** The codec resets the response before applying the
   reply, so one request/response pair serves every iteration.
3. **Bucket with `pgrpc.StatusOf`.** See above.

## Why this is a separate module

It is the only module here shaped like a consumer, so its dependency graph is
the claim *"a user links the runtime, not the plugin, and never grpc-go"* — and
a claim in a README is worth nothing without a check. CI runs one:

```bash
go list -deps ./... | grep -E 'compiler/protogen|types/pluginpb|^google\.golang\.org/grpc'
```

must find nothing. Today the binary links 27 protobuf packages and 5 poseidon
ones, and neither the code generator nor grpc-go.

It reuses `testdata`'s schema rather than carrying its own, so the example can
never drift from what the generator actually produces.

## Flags

| Flag | Default | |
|---|---|---|
| `-addr` | `localhost:50051` | server address |
| `-users` | 32 | concurrent virtual users |
| `-conns` | 1 | HTTP/2 connections to spread users across |
| `-duration` | 10s | how long to run |
| `-plaintext` | false | h2c: no TLS, prior knowledge |
| `-insecure` | false | skip TLS certificate verification |
| `-name` | `world` | the name to greet |

Ctrl-C stops the run and still prints the report — an interrupted run is
usually the one whose numbers you wanted.
