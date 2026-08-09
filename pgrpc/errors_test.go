package pgrpc_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/lodgvideon/poseidon-http-client/conn"
	"github.com/lodgvideon/poseidon-http-client/grpc"
	"github.com/lodgvideon/protoc-gen-go-poseidon/pgrpc"
)

// TestCodecErrorSatisfiesBothIdioms is the whole reason CodecError has a
// two-element Unwrap. A caller bucketing by status and a caller matching their
// codec's sentinel must both work off the same value.
func TestCodecErrorSatisfiesBothIdioms(t *testing.T) {
	cause := errors.New("field 3 is not a valid UTF-8 string")
	err := pgrpc.NewCodecError(pgrpc.OpMarshal, "proto", struct{ X int }{}, cause)

	var st *pgrpc.Status
	if !errors.As(err, &st) {
		t.Fatal("errors.As found no *Status")
	}
	if st.Code != pgrpc.Internal {
		t.Errorf("Code = %v, want Internal", st.Code)
	}
	if !errors.Is(err, cause) {
		t.Error("errors.Is lost the codec's own cause")
	}
}

// TestCodecErrorRecordsTypeWithoutRetainingMessage pins the doc claim on the
// Type field. Holding the message would keep an entire request or response
// alive for as long as anyone holds the error.
func TestCodecErrorRecordsTypeWithoutRetainingMessage(t *testing.T) {
	type request struct{ Body []byte }
	err := pgrpc.NewCodecError(pgrpc.OpUnmarshal, "vt", &request{Body: []byte("x")}, io.ErrUnexpectedEOF)

	if !strings.Contains(err.Type, "request") {
		t.Errorf("Type = %q, want it to name the Go type", err.Type)
	}
	if !strings.Contains(err.Error(), "unmarshal") {
		t.Errorf("Error() does not name the operation: %v", err)
	}
	if !strings.Contains(err.Error(), `"vt"`) {
		t.Errorf("Error() does not name the codec: %v", err)
	}
}

func TestCodecErrorWithNilCause(t *testing.T) {
	err := pgrpc.NewCodecError(pgrpc.OpMarshal, "proto", 0, nil)
	var st *pgrpc.Status
	if !errors.As(err, &st) {
		t.Fatal("errors.As found no *Status when the cause was nil")
	}
	if st.Code != pgrpc.Internal {
		t.Errorf("Code = %v, want Internal", st.Code)
	}
}

func TestGuardAdmitsOneHolder(t *testing.T) {
	var g pgrpc.Guard
	if err := g.Enter(); err != nil {
		t.Fatalf("first Enter: %v", err)
	}
	if err := g.Enter(); !errors.Is(err, pgrpc.ErrCallerInUse) {
		t.Errorf("second Enter = %v, want ErrCallerInUse", err)
	}
	g.Leave()
	if err := g.Enter(); err != nil {
		t.Errorf("Enter after Leave: %v", err)
	}
}

// TestGuardUnderContention checks the CAS rather than the happy path: with N
// goroutines racing, exactly one may hold the guard at a time. Run with -race.
func TestGuardUnderContention(t *testing.T) {
	var (
		g       pgrpc.Guard
		wg      sync.WaitGroup
		mu      sync.Mutex
		holders int
		maxSeen int
		admits  int
	)
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := g.Enter(); err != nil {
				return
			}
			mu.Lock()
			admits++
			holders++
			if holders > maxSeen {
				maxSeen = holders
			}
			mu.Unlock()

			mu.Lock()
			holders--
			mu.Unlock()
			g.Leave()
		}()
	}
	wg.Wait()

	if maxSeen > 1 {
		t.Errorf("%d goroutines held the guard at once, want at most 1", maxSeen)
	}
	if admits == 0 {
		t.Error("nobody ever entered the guard")
	}
}

func TestStatusOf(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want pgrpc.Code
	}{
		{"nil", nil, pgrpc.OK},
		{"a real status wins", (&pgrpc.Status{Code: pgrpc.NotFound}), pgrpc.NotFound},
		{"a wrapped status still wins", fmt.Errorf("rpc: %w", &pgrpc.Status{Code: pgrpc.Aborted}), pgrpc.Aborted},
		{"deadline", context.DeadlineExceeded, pgrpc.DeadlineExceeded},
		{"cancel", context.Canceled, pgrpc.Canceled},
		{"wrapped cancel", fmt.Errorf("send: %w", context.Canceled), pgrpc.Canceled},
		{"message too large", grpc.ErrMessageTooLarge, pgrpc.ResourceExhausted},
		{"peer compressed", grpc.ErrCompressed, pgrpc.Internal},
		{"conn closed", conn.ErrConnClosed, pgrpc.Unavailable},
		{"conn draining", conn.ErrConnDraining, pgrpc.Unavailable},
		{"goaway", conn.ErrGoAway, pgrpc.Unavailable},
		{"too many streams", conn.ErrTooManyStreams, pgrpc.Unavailable},
		{"we closed the stream", grpc.ErrStreamClosed, pgrpc.Canceled},
		{"we closed the send side", grpc.ErrSendClosed, pgrpc.Canceled},
		{"peer reset the stream", conn.ErrStreamClosed, pgrpc.Unavailable},
		{"bare EOF", io.EOF, pgrpc.Internal},
		{"anything else", errors.New("who knows"), pgrpc.Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pgrpc.StatusOf(tc.err).Code; got != tc.want {
				t.Errorf("StatusOf(%v).Code = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestStatusOfNeverSilentlyReportsOK is the property that matters more than any
// individual row: a non-nil error must never classify as OK, or a dying
// connection shows up in a load run's numbers as success.
func TestStatusOfNeverSilentlyReportsOK(t *testing.T) {
	for _, err := range []error{
		context.Canceled, context.DeadlineExceeded,
		grpc.ErrMessageTooLarge, grpc.ErrCompressed,
		grpc.ErrStreamClosed, grpc.ErrSendClosed, grpc.ErrBadMethod,
		grpc.ErrInvalidMetadata, grpc.ErrReservedMetadata,
		conn.ErrConnClosed, conn.ErrConnDraining, conn.ErrGoAway,
		conn.ErrTooManyStreams, conn.ErrStreamClosed,
		io.EOF, io.ErrUnexpectedEOF,
		errors.New("unrecognised"),
		pgrpc.ErrCallerInUse, pgrpc.ErrSendInFlight, pgrpc.ErrRecvInFlight,
		pgrpc.NewCodecError(pgrpc.OpMarshal, "proto", 0, errors.New("x")),
	} {
		if got := pgrpc.StatusOf(err); got.Code == pgrpc.OK {
			t.Errorf("StatusOf(%v) classified a failure as OK", err)
		}
	}
}

// TestStatusOfDistinguishesTheTwoStreamClosed guards the pair most likely to be
// collapsed by a careless edit: grpc.ErrStreamClosed means this client tore the
// stream down (retrying will not help), conn.ErrStreamClosed means the peer did
// (reconnecting might).
func TestStatusOfDistinguishesTheTwoStreamClosed(t *testing.T) {
	ours := pgrpc.StatusOf(grpc.ErrStreamClosed).Code
	theirs := pgrpc.StatusOf(conn.ErrStreamClosed).Code
	if ours == theirs {
		t.Fatalf("both ErrStreamClosed sentinels map to %v; they mean opposite things", ours)
	}
	if ours != pgrpc.Canceled {
		t.Errorf("grpc.ErrStreamClosed = %v, want Canceled", ours)
	}
	if theirs != pgrpc.Unavailable {
		t.Errorf("conn.ErrStreamClosed = %v, want Unavailable", theirs)
	}
}
