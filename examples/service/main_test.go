package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lodgvideon/poseidon-http-client/conn"
	"github.com/lodgvideon/protoc-gen-go-poseidon/pgrpc"
	"github.com/lodgvideon/protoc-gen-go-poseidon/testdata/helloworld"
	"github.com/lodgvideon/protoc-gen-go-poseidon/testdata/helloworld/poseidon"
)

// This is the payoff of the generated INTERFACE, and it is the reason a service
// should hold GreeterClient rather than *pgrpc.Client: a handler test needs no
// socket, no server and no goroutine.
//
// It covers the unary handler only. The streaming one cannot be faked from
// here — a *pgrpc.ServerStream can only come from a real connection, because
// poseidon exposes no constructor for the grpc.Stream underneath it — so its
// behaviour is covered in test/e2e against a real grpc-go server instead. That
// split is documented on pgrpc.StreamInvoker rather than discovered.

type fakeGreeter struct {
	reply *helloworld.HelloReply
	err   error
	// md records what the handler attached, so a metadata test asserts on the
	// call rather than on the handler's intentions.
	md []conn.HeaderField
}

func (f *fakeGreeter) SayHello(_ context.Context, _ *helloworld.HelloRequest,
	opts ...pgrpc.CallOption) (*helloworld.HelloReply, error) {
	var cfg pgrpc.CallConfig
	cfg.Apply(opts...)
	f.md = cfg.Metadata()
	return f.reply, f.err
}

func (f *fakeGreeter) LotsOfReplies(context.Context, *helloworld.HelloRequest,
	...pgrpc.CallOption) (*pgrpc.ServerStream[helloworld.HelloReply], error) {
	return nil, errors.New("a stream cannot be faked; see test/e2e")
}

func (f *fakeGreeter) LotsOfGreetings(context.Context,
	...pgrpc.CallOption) (*pgrpc.ClientStream[helloworld.HelloRequest, helloworld.HelloReply], error) {
	return nil, errors.New("a stream cannot be faked; see test/e2e")
}

func (f *fakeGreeter) BidiHello(context.Context,
	...pgrpc.CallOption) (*pgrpc.BidiStream[helloworld.HelloRequest, helloworld.HelloReply], error) {
	return nil, errors.New("a stream cannot be faked; see test/e2e")
}

var _ poseidon.GreeterClient = (*fakeGreeter)(nil)

func TestGreetReturnsTheReply(t *testing.T) {
	f := &fakeGreeter{reply: &helloworld.HelloReply{Message: "hello world"}}
	w := httptest.NewRecorder()

	(&server{greeter: f}).greet(w, httptest.NewRequest(http.MethodGet, "/greet?name=world", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := strings.TrimSpace(w.Body.String()); got != "hello world" {
		t.Errorf("body = %q", got)
	}
}

func TestGreetForwardsTheCallersCredential(t *testing.T) {
	f := &fakeGreeter{reply: &helloworld.HelloReply{Message: "ok"}}
	r := httptest.NewRequest(http.MethodGet, "/greet?name=world", nil)
	r.Header.Set("Authorization", "Bearer t0ken")

	(&server{greeter: f}).greet(httptest.NewRecorder(), r)

	if len(f.md) != 1 {
		t.Fatalf("the call carried %d metadata entries, want 1", len(f.md))
	}
	if k := string(f.md[0].Name); k != "authorization" {
		t.Errorf("key = %q, want it lowercased by AppendMetadata", k)
	}
	if v := string(f.md[0].Value); v != "Bearer t0ken" {
		t.Errorf("value = %q", v)
	}
}

func TestGreetSendsNoAuthWhenTheCallerSentNone(t *testing.T) {
	f := &fakeGreeter{reply: &helloworld.HelloReply{Message: "ok"}}
	(&server{greeter: f}).greet(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/greet?name=world", nil))

	if len(f.md) != 0 {
		t.Errorf("an anonymous request carried %d metadata entries", len(f.md))
	}
}

// TestGreetMapsTheStatus is the table a service actually needs. The last row is
// the one worth having: an error carrying NO status at all — a dead connection,
// a refused stream — must not become a 200, which is what happens to code that
// reaches for errors.As and takes the zero value.
func TestGreetMapsTheStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"not found", &pgrpc.Status{Code: pgrpc.NotFound}, http.StatusNotFound},
		{"unauthenticated", &pgrpc.Status{Code: pgrpc.Unauthenticated}, http.StatusUnauthorized},
		{"permission denied", &pgrpc.Status{Code: pgrpc.PermissionDenied}, http.StatusForbidden},
		{"invalid argument", &pgrpc.Status{Code: pgrpc.InvalidArgument}, http.StatusBadRequest},
		{"resource exhausted", &pgrpc.Status{Code: pgrpc.ResourceExhausted}, http.StatusTooManyRequests},
		{"unavailable", &pgrpc.Status{Code: pgrpc.Unavailable}, http.StatusServiceUnavailable},
		{"internal", &pgrpc.Status{Code: pgrpc.Internal}, http.StatusBadGateway},
		{"a transport error with no status", errors.New("connection reset"), http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			(&server{greeter: &fakeGreeter{err: tc.err}}).greet(w,
				httptest.NewRequest(http.MethodGet, "/greet?name=x", nil))

			if w.Code != tc.want {
				t.Errorf("status = %d, want %d", w.Code, tc.want)
			}
			if w.Code == http.StatusOK {
				t.Error("a failed call was reported as a success")
			}
		})
	}
}

// TestTheBackendMessageDoesNotReachTheCaller keeps internal detail internal. On
// the Trailers-Only shape the message is whatever the backend felt like saying,
// and it routinely names hosts, queries or identifiers.
func TestTheBackendMessageDoesNotReachTheCaller(t *testing.T) {
	const secret = "user 4711 not in shard db-7.internal"
	w := httptest.NewRecorder()

	(&server{greeter: &fakeGreeter{err: &pgrpc.Status{Code: pgrpc.NotFound, Message: secret}}}).
		greet(w, httptest.NewRequest(http.MethodGet, "/greet?name=x", nil))

	if strings.Contains(w.Body.String(), secret) {
		t.Errorf("the backend's message reached the caller: %q", w.Body.String())
	}
}
