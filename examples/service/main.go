// Command service is an ordinary HTTP service that happens to call a gRPC
// backend through a generated poseidon client.
//
// It exists because the other example is a load generator, and a load generator
// is an unusual program: one Caller per virtual user, buffers reused, outcomes
// bucketed. Almost nobody writes that. This is what the normal case looks like,
// and the differences are the point:
//
//  1. Use the ERGONOMIC face, GreeterClient. It allocates a response per call
//     and resolves options per call — three allocations, measured. A service
//     doing one RPC per HTTP request will not notice, and reaching for the
//     buffer-reusing Caller here buys nothing while costing you a type that
//     serves exactly one goroutine at a time.
//  2. Hold ONE client, injected. It is immutable after construction and safe
//     for any number of goroutines, so a package-level dial in init() buys
//     nothing over a field and loses you the ability to test.
//  3. Derive every call's context from the REQUEST's. That is what makes a
//     hung backend release your handler when the client hangs up, and it is
//     what puts a grpc-timeout on the wire so the server stops working too.
//  4. Classify with pgrpc.StatusOf, then map to HTTP. A call that never
//     reached the server has no *Status at all — treating a missing one as the
//     zero value reports a dead backend as 200.
//
// Run it against the e2e test server, or any grpc-go Greeter:
//
//	service -backend localhost:50051 -plaintext -addr :8080
//	curl 'localhost:8080/greet?name=world'
//	curl 'localhost:8080/greetings?name=world'
//	curl --data-binary 'ann\nbob' localhost:8080/greet-many
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lodgvideon/poseidon-http-client/conn"
	poseidongrpc "github.com/lodgvideon/poseidon-http-client/grpc"
	"github.com/lodgvideon/protoc-gen-go-poseidon/pgrpc"
	"github.com/lodgvideon/protoc-gen-go-poseidon/testdata/helloworld"
	"github.com/lodgvideon/protoc-gen-go-poseidon/testdata/helloworld/poseidon"
)

// callTimeout bounds one backend call.
//
// It is deliberately shorter than any sane upstream timeout. The context
// deadline becomes the grpc-timeout header, so the SERVER gives up on its own
// work when this expires rather than finishing a response nobody is waiting
// for.
const callTimeout = 3 * time.Second

// maxUploadBytes bounds a client-streaming request body. Without it an
// anonymous caller decides how much this service forwards to its backend.
const maxUploadBytes = 1 << 20

func main() {
	var (
		addr      = flag.String("addr", ":8080", "HTTP listen address")
		backend   = flag.String("backend", "localhost:50051", "gRPC backend address")
		plaintext = flag.Bool("plaintext", false, "h2c: no TLS, prior knowledge")
		insecure  = flag.Bool("insecure", false, "skip TLS certificate verification")
	)
	flag.Parse()

	if err := run(*addr, *backend, *plaintext, *insecure); err != nil {
		slog.Error("service failed", "err", err)
		os.Exit(1)
	}
}

func run(addr, backend string, plaintext, insecure bool) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cc, err := dial(dialCtx, backend, plaintext, insecure)
	if err != nil {
		return fmt.Errorf("dial %s: %w", backend, err)
	}
	defer func() { _ = cc.Close() }()

	// One client for the process. NewGreeterClientOn builds the pgrpc.Client
	// and supplies the protobuf codec, so this is the whole wiring.
	svc := &server{greeter: poseidon.NewGreeterClientOn(cc)}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /greet", svc.greet)
	mux.HandleFunc("GET /greetings", svc.greetings)
	mux.HandleFunc("POST /greet-many", svc.greetMany)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", addr, "backend", backend)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	// Stop accepting, drain in-flight handlers, and only then let the deferred
	// Close tear the gRPC connection down — closing it first would fail the
	// very requests this shutdown is trying to finish.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelShutdown()
	return srv.Shutdown(shutdownCtx)
}

// server holds the dependencies. A field rather than a package-level variable,
// so a test can hand it a different GreeterClient — the generated interface is
// what makes that possible.
type server struct {
	greeter poseidon.GreeterClient
}

// greet is the unary shape: one request, one reply.
func (s *server) greet(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), callTimeout)
	defer cancel()

	reply, err := s.greeter.SayHello(ctx,
		&helloworld.HelloRequest{Name: r.URL.Query().Get("name")},
		forwardAuth(r)...)
	if err != nil {
		writeStatus(w, err)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// Nothing to be done with a write error here: the status line is already
	// out, and the only reader who could learn about it is the one who left.
	_, _ = fmt.Fprintln(w, reply.GetMessage())
}

// greetings is the server-streaming shape, consumed with the self-closing
// iterator.
//
// The iterator closes the stream on every exit — a normal end, an error, an
// early return, a panic — which is what makes this handler leak-free without a
// defer of its own. An abandoned stream holds a slot against the connection's
// concurrent-stream limit, so on a busy service that is not a tidiness point.
func (s *server) greetings(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), callTimeout)
	defer cancel()

	stream, err := s.greeter.LotsOfReplies(ctx,
		&helloworld.HelloRequest{Name: r.URL.Query().Get("name")},
		forwardAuth(r)...)
	if err != nil {
		writeStatus(w, err)
		return
	}

	// Headers go out before the first reply, so a failure after this point can
	// no longer change the status code. That is inherent to streaming a
	// response, not something this client adds.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	flusher, _ := w.(http.Flusher)

	for reply, err := range stream.All(ctx) {
		if err != nil {
			// Too late for a status code. Log it and stop; the client sees a
			// truncated body, which is the honest signal available here.
			slog.Error("stream failed midway", "err", err, "code", pgrpc.StatusOf(err).Code)
			return
		}
		if _, err := fmt.Fprintln(w, reply.GetMessage()); err != nil {
			// The caller hung up. Returning here does more than tidy up: the
			// iterator closes the stream on the way out, which tells the
			// backend to stop producing a response nobody will read. Draining
			// it to the end instead would bill the backend for the whole thing.
			slog.Info("client went away mid-stream", "err", err)
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}

	// Valid after the iterator closed the stream: the terminal status and
	// trailers are snapshotted when it ends, not read live.
	if st := stream.Status(); st.Code != pgrpc.OK {
		slog.Warn("stream ended non-OK", "code", st.Code, "message", st.Message)
	}
}

// greetMany is the client-streaming shape: many requests, one reply.
//
// It is the natural fit for an upload — a request body streamed straight into
// the backend without being buffered first — which is the case a service
// actually has for this shape.
//
// Note what is NOT here: a Recv loop. ClientStream has no Recv method at all,
// because the natural mistake on this shape is to read in one goroutine while
// another sends, which puts two goroutines inside poseidon's unguarded receive
// state. The reply comes back from the call that ends the sending side.
func (s *server) greetMany(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), callTimeout)
	defer cancel()

	stream, err := s.greeter.LotsOfGreetings(ctx, forwardAuth(r)...)
	if err != nil {
		writeStatus(w, err)
		return
	}
	// Unlike the server-streaming iterator, nothing closes this one for you:
	// the send side is yours to end, so the stream is yours to close.
	defer func() { _ = stream.Close() }()

	// Bound what an unauthenticated caller can make you forward.
	body := bufio.NewScanner(io.LimitReader(r.Body, maxUploadBytes))
	for body.Scan() {
		name := strings.TrimSpace(body.Text())
		if name == "" {
			continue
		}
		if err := stream.Send(ctx, &helloworld.HelloRequest{Name: name}); err != nil {
			writeStatus(w, err)
			return
		}
	}
	if err := body.Err(); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}

	// CloseAndRecv half-closes and reads the single reply. An empty body is
	// not a special case: the call still has to be ended properly, or the
	// backend waits out its deadline for a request that is never coming.
	//
	// SendLastAndRecv is the faster form — it folds the half-close into the
	// last message's own DATA frame, saving a flush, a TLS record and usually
	// a segment. It is not used here because using it means holding the final
	// message back through the loop, and that off-by-one is logic this
	// example's own tests cannot reach: a *pgrpc.ClientStream only comes from
	// a real connection. An example should not carry logic it cannot prove.
	reply := &helloworld.HelloReply{}
	if err := stream.CloseAndRecv(ctx, reply); err != nil {
		writeStatus(w, err)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = fmt.Fprintln(w, reply.GetMessage())
}

// forwardAuth passes the caller's credential through to the backend.
//
// It returns options rather than a metadata slice because this is the
// ergonomic path, where one allocation per header is beside the point.
//
// poseidon marks authorization, proxy-authorization and cookie never-indexed by
// itself, so the token does not enter the connection's shared HPACK dynamic
// table. For any OTHER credential-bearing header, say so explicitly with a
// field carrying pgrpc.IndexNever — the automatic list is a floor, not a
// guarantee.
func forwardAuth(r *http.Request) []pgrpc.CallOption {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return nil
	}
	return []pgrpc.CallOption{pgrpc.WithHeaderString("authorization", auth)}
}

// writeStatus maps a gRPC outcome onto an HTTP one.
//
// pgrpc.StatusOf, not errors.As. A call that never reached the server — a dead
// connection, a cancelled context, a refused stream — carries no *Status at
// all, and code that assumes one takes the zero value, whose code is OK. That
// reports a dead backend as 200.
func writeStatus(w http.ResponseWriter, err error) {
	st := pgrpc.StatusOf(err)
	code := httpStatus(st.Code)

	// The backend's message is for your logs, not for the caller: it can carry
	// internal detail, and on the Trailers-Only shape it is chosen by whatever
	// the backend felt like saying.
	slog.Error("backend call failed", "code", st.Code, "message", st.Message, "http", code)
	http.Error(w, strings.ToLower(http.StatusText(code)), code)
}

// httpStatus is the gRPC-to-HTTP mapping, kept in one place so two handlers
// cannot disagree.
func httpStatus(c pgrpc.Code) int {
	switch c {
	case pgrpc.OK:
		return http.StatusOK
	case pgrpc.InvalidArgument, pgrpc.FailedPrecondition, pgrpc.OutOfRange:
		return http.StatusBadRequest
	case pgrpc.Unauthenticated:
		return http.StatusUnauthorized
	case pgrpc.PermissionDenied:
		return http.StatusForbidden
	case pgrpc.NotFound:
		return http.StatusNotFound
	case pgrpc.AlreadyExists, pgrpc.Aborted:
		return http.StatusConflict
	case pgrpc.ResourceExhausted:
		return http.StatusTooManyRequests
	case pgrpc.Canceled:
		// 499, nginx's "client closed request". The caller is gone, so the
		// number is for your dashboards rather than for them.
		return 499
	case pgrpc.DeadlineExceeded:
		return http.StatusGatewayTimeout
	case pgrpc.Unimplemented:
		return http.StatusNotImplemented
	case pgrpc.Unavailable:
		return http.StatusServiceUnavailable
	default:
		// Unknown, Internal, DataLoss.
		return http.StatusBadGateway
	}
}

// dial opens the one connection this process uses.
//
// A poseidon ClientConn multiplexes as many concurrent calls as the server's
// SETTINGS_MAX_CONCURRENT_STREAMS allows, so one is the normal unit for a
// service. There is no pool to configure — and if you outgrow one connection,
// what you want is several ClientConns behind your own pgrpc.Invoker, not a
// knob here.
func dial(ctx context.Context, addr string, plaintext, insecure bool) (*poseidongrpc.ClientConn, error) {
	opts := poseidongrpc.Options{
		Conn: conn.ConnOptions{
			// The first thing to turn on for anything crossing a real network.
			// Both HTTP/2 windows start at 64 KiB and stay there until an
			// endpoint says otherwise, which caps the whole connection at
			// roughly 6.5 MB/s at 10 ms RTT. Loopback hides this completely, so
			// a local test will never show you the ceiling a staging run does.
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
