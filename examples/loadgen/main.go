// Command loadgen drives a gRPC method through a generated poseidon client and
// reports throughput, latency and outcome buckets.
//
// It is small on purpose. What it demonstrates is the shape a load generator
// should take with this client, and that shape is three rules:
//
//  1. ONE Caller per virtual user, built once outside the request loop. A
//     Caller owns its request scratch, its response scratch and its resolved
//     call configuration; sharing one across goroutines is what
//     pgrpc.ErrCallerInUse exists to report.
//  2. Reuse the request and response MESSAGES too. The codec resets the
//     response before applying the reply, so the same pair serves every
//     iteration.
//  3. Bucket outcomes with pgrpc.StatusOf, never with errors.As alone. poseidon
//     returns a *Status for anything that reached the server and a bare
//     transport error for anything that did not — a dead connection has no
//     status, and code that assumes one counts it as a success.
//
// Usage:
//
//	loadgen -addr localhost:50051 -plaintext -users 64 -duration 10s
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/lodgvideon/poseidon-http-client/conn"
	poseidongrpc "github.com/lodgvideon/poseidon-http-client/grpc"
	"github.com/lodgvideon/protoc-gen-go-poseidon/pgrpc"
	"github.com/lodgvideon/protoc-gen-go-poseidon/pgrpc/protocodec"
	"github.com/lodgvideon/protoc-gen-go-poseidon/testdata/helloworld"
	"github.com/lodgvideon/protoc-gen-go-poseidon/testdata/helloworld/poseidon"
)

func main() {
	var (
		addr      = flag.String("addr", "localhost:50051", "server address")
		users     = flag.Int("users", 32, "concurrent virtual users")
		conns     = flag.Int("conns", 1, "HTTP/2 connections to spread users across")
		duration  = flag.Duration("duration", 10*time.Second, "how long to run")
		plaintext = flag.Bool("plaintext", false, "h2c: no TLS, prior knowledge")
		insecure  = flag.Bool("insecure", false, "skip TLS certificate verification")
		name      = flag.String("name", "world", "the name to greet")
	)
	flag.Parse()

	if err := run(*addr, *users, *conns, *duration, *plaintext, *insecure, *name); err != nil {
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(1)
	}
}

func run(addr string, users, conns int, duration time.Duration, plaintext, insecure bool, name string) error {
	if users < 1 || conns < 1 {
		return errors.New("users and conns must be at least 1")
	}

	// Ctrl-C stops the run and still prints the report, because a run you had
	// to interrupt is exactly the one whose numbers you wanted.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()

	clients := make([]*pgrpc.Client, conns)
	for i := range clients {
		cc, err := dial(ctx, addr, plaintext, insecure)
		if err != nil {
			return fmt.Errorf("dial %s: %w", addr, err)
		}
		defer func() { _ = cc.Close() }()
		clients[i] = pgrpc.NewClient(cc, pgrpc.WithCodec(protocodec.Codec{}))
	}

	results := make([]*bucket, users)
	var wg sync.WaitGroup
	start := time.Now()

	for u := range users {
		b := &bucket{codes: map[pgrpc.Code]int{}}
		results[u] = b
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Everything a virtual user needs, allocated once. Nothing below
			// this line allocates in the client layer.
			caller := poseidon.NewGreeterCaller(clients[u%len(clients)])
			in := &helloworld.HelloRequest{Name: name}
			out := &helloworld.HelloReply{}

			for ctx.Err() == nil {
				t0 := time.Now()
				err := caller.SayHello(ctx, in, out)
				b.record(time.Since(t0), err)
			}
		}()
	}

	wg.Wait()
	report(merge(results), time.Since(start))
	return nil
}

// dial opens one poseidon connection.
func dial(ctx context.Context, addr string, plaintext, insecure bool) (*poseidongrpc.ClientConn, error) {
	opts := poseidongrpc.Options{
		Conn: conn.ConnOptions{
			// Off by default in poseidon and off here. A gRPC server enforces a
			// minimum ping interval and answers a faster one with
			// GOAWAY(ENHANCE_YOUR_CALM) after two strikes.
			KeepaliveInterval: 0,
			// The first thing to turn on for anything crossing a real network:
			// both HTTP/2 windows start at 64 KiB and stay there until an
			// endpoint says otherwise, which caps the whole connection at
			// roughly 6.5 MB/s at 10 ms RTT. Loopback hides this completely.
			AutoTuneRecvWindow: true,
		},
	}
	if plaintext {
		opts.Scheme = "http"
		opts.Conn.Dialer = &conn.PlaintextDialer{}
	} else {
		opts.Conn.Dialer = &conn.TLSDialer{Config: &tls.Config{
			NextProtos:         []string{"h2"},
			InsecureSkipVerify: insecure, //nolint:gosec // opt-in via -insecure
		}}
	}
	return poseidongrpc.Dial(ctx, addr, opts)
}

// bucket is one virtual user's tally. Per-user, so the hot loop never touches a
// shared counter and the run measures the client rather than contention on its
// own statistics.
type bucket struct {
	latencies []time.Duration
	codes     map[pgrpc.Code]int
}

func (b *bucket) record(d time.Duration, err error) {
	b.latencies = append(b.latencies, d)
	// StatusOf, not errors.As. A call that never reached the server has no
	// *Status at all, and treating a missing one as the zero value files a dead
	// connection under OK.
	b.codes[pgrpc.StatusOf(err).Code]++
}

func merge(bs []*bucket) *bucket {
	out := &bucket{codes: map[pgrpc.Code]int{}}
	for _, b := range bs {
		out.latencies = append(out.latencies, b.latencies...)
		for c, n := range b.codes {
			out.codes[c] += n
		}
	}
	return out
}

func report(b *bucket, elapsed time.Duration) {
	total := len(b.latencies)
	if total == 0 {
		fmt.Println("no requests completed")
		return
	}
	sort.Slice(b.latencies, func(i, j int) bool { return b.latencies[i] < b.latencies[j] })

	fmt.Printf("requests   %d in %s (%.0f/s)\n", total, elapsed.Round(time.Millisecond),
		float64(total)/elapsed.Seconds())
	fmt.Printf("latency    p50 %s  p90 %s  p99 %s  max %s\n",
		pct(b.latencies, 0.50), pct(b.latencies, 0.90),
		pct(b.latencies, 0.99), b.latencies[total-1].Round(time.Microsecond))

	// Sorted, so two runs are diffable.
	codes := make([]pgrpc.Code, 0, len(b.codes))
	for c := range b.codes {
		codes = append(codes, c)
	}
	sort.Slice(codes, func(i, j int) bool { return codes[i] < codes[j] })

	fmt.Println("outcomes")
	for _, c := range codes {
		n := b.codes[c]
		fmt.Printf("  %-20s %8d  %5.1f%%\n", c, n, 100*float64(n)/float64(total))
	}
}

// pct returns the p-th percentile of a sorted slice, nearest-rank.
func pct(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p * float64(len(sorted)))
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i].Round(time.Microsecond)
}
