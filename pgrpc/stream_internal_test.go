package pgrpc

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/lodgvideon/poseidon-http-client/conn"
)

// The wire behaviour of these types needs a real server, because *grpc.Stream
// has no exported constructor — that lands in the end-to-end suite. What can be
// pinned here is the logic that decides what a caller is TOLD, which is where a
// wrong answer is silent: a status that reads OK for a failed call, or a guard
// that admits a second goroutine into poseidon's unguarded receive state.

// TestStatusNeverReportsFalseOK is the whole reason Status is not a pass-through
// to poseidon's.
//
// poseidon populates its own status only from peer-DECLARED outcomes. A context
// cancellation, a dead connection, a truncated message — all of those land in
// its error field and leave the status at its zero value, whose code is OK.
// Forwarding that verbatim puts every transport failure in a load generator's
// success bucket.
func TestStatusNeverReportsFalseOK(t *testing.T) {
	for _, tc := range []struct {
		name       string
		termErr    error
		termStatus Status
		want       Code
	}{
		{"clean end", nil, Status{Code: OK}, OK},
		{"io.EOF is a clean end", io.EOF, Status{Code: OK}, OK},
		{"peer declared a failure", io.EOF, Status{Code: NotFound}, NotFound},
		{"peer status outranks the error", errors.New("x"), Status{Code: PermissionDenied}, PermissionDenied},
		{"context cancelled", context.Canceled, Status{}, Canceled},
		{"deadline", context.DeadlineExceeded, Status{}, DeadlineExceeded},
		{"connection gone", conn.ErrConnClosed, Status{}, Unavailable},
		{"peer reset", conn.ErrStreamClosed, Status{}, Unavailable},
		{"unclassifiable", errors.New("who knows"), Status{}, Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &baseStream{ended: true, termErr: tc.termErr, termStatus: tc.termStatus}
			if got := b.Status().Code; got != tc.want {
				t.Errorf("Status().Code = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestStatusBeforeEndIsDistinguishableFromOK covers the one case where a zero
// Status is legitimate. Ended is what tells them apart, and without it a caller
// bucketing an in-flight stream would count it as a success.
func TestStatusBeforeEndIsDistinguishableFromOK(t *testing.T) {
	b := &baseStream{}
	if b.Ended() {
		t.Fatal("a fresh stream reports Ended")
	}
	if got := b.Status().Code; got != OK {
		t.Errorf("Status().Code = %v before the end", got)
	}

	b.ended, b.termStatus = true, Status{Code: OK}
	if !b.Ended() {
		t.Error("Ended is false after the stream ended")
	}
}

func TestTerminalLatchesAndReportsEOFForACleanEnd(t *testing.T) {
	b := &baseStream{}
	if err := b.terminal(); err != nil {
		t.Errorf("terminal() = %v on a running stream, want nil", err)
	}

	b.ended = true
	if err := b.terminal(); !errors.Is(err, io.EOF) {
		t.Errorf("terminal() = %v after a clean end, want io.EOF", err)
	}

	boom := errors.New("reset")
	b.termErr = boom
	if err := b.terminal(); !errors.Is(err, boom) {
		t.Errorf("terminal() = %v, want the latched error", err)
	}
}

// TestRecvGuardAdmitsOne is what stands between a caller bug and a silent data
// race on poseidon's receive state.
func TestRecvGuardAdmitsOne(t *testing.T) {
	b := &baseStream{}
	if err := b.enterRecv(); err != nil {
		t.Fatalf("first enterRecv: %v", err)
	}
	if err := b.enterRecv(); !errors.Is(err, ErrRecvInFlight) {
		t.Errorf("second enterRecv = %v, want ErrRecvInFlight", err)
	}
	b.leaveRecv()
	if err := b.enterRecv(); err != nil {
		t.Errorf("enterRecv after leaveRecv: %v", err)
	}
}

func TestSendGuardAdmitsOne(t *testing.T) {
	var s sendSide
	if err := s.enterSend(); err != nil {
		t.Fatalf("first enterSend: %v", err)
	}
	if err := s.enterSend(); !errors.Is(err, ErrSendInFlight) {
		t.Errorf("second enterSend = %v, want ErrSendInFlight", err)
	}
	s.leaveSend()
	if err := s.enterSend(); err != nil {
		t.Errorf("enterSend after leaveSend: %v", err)
	}
}

// TestGuardsUnderContention exercises the CAS rather than the happy path. Run
// with -race.
func TestGuardsUnderContention(t *testing.T) {
	b := &baseStream{}
	var s sendSide
	var wg sync.WaitGroup
	var mu sync.Mutex
	recvHeld, sendHeld, maxRecv, maxSend := 0, 0, 0, 0

	for range 64 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if b.enterRecv() != nil {
				return
			}
			mu.Lock()
			recvHeld++
			maxRecv = max(maxRecv, recvHeld)
			recvHeld--
			mu.Unlock()
			b.leaveRecv()
		}()
		go func() {
			defer wg.Done()
			if s.enterSend() != nil {
				return
			}
			mu.Lock()
			sendHeld++
			maxSend = max(maxSend, sendHeld)
			sendHeld--
			mu.Unlock()
			s.leaveSend()
		}()
	}
	wg.Wait()

	if maxRecv > 1 {
		t.Errorf("%d goroutines were inside the receive guard at once", maxRecv)
	}
	if maxSend > 1 {
		t.Errorf("%d goroutines were inside the send guard at once", maxSend)
	}
}

// TestSendTolerance pins which send failures may fall through to the receive
// side. Getting this wrong discards a complete PERMISSION_DENIED already
// buffered and reports a transport failure instead — and Trailers-Only is, in
// poseidon's own words, how gRPC servers report most errors.
func TestSendTolerance(t *testing.T) {
	if err := sendTolerant(nil); err != nil {
		t.Errorf("sendTolerant(nil) = %v", err)
	}
	if err := sendTolerant(conn.ErrStreamClosed); err != nil {
		t.Errorf("sendTolerant(peer half-close) = %v, want nil — the answer may already be buffered", err)
	}
	boom := errors.New("connection reset")
	if err := sendTolerant(boom); !errors.Is(err, boom) {
		t.Errorf("sendTolerant(%v) = %v, want it fatal", boom, err)
	}
	ce := NewCodecError(OpMarshal, "proto", 0, errors.New("bad"))
	if err := sendTolerant(ce); err == nil {
		t.Error("a marshal failure was tolerated; nothing is coming back from one")
	}
}

func TestHeaderIsNotReadyBeforeItArrives(t *testing.T) {
	b := &baseStream{}
	if _, err := b.Header(context.Background()); !errors.Is(err, ErrHeaderNotReady) {
		t.Errorf("Header = %v, want ErrHeaderNotReady", err)
	}

	want := []conn.HeaderField{{Name: []byte("x"), Value: []byte("1")}}
	b.hdr, b.hdrDone = want, true
	got, err := b.Header(context.Background())
	if err != nil {
		t.Fatalf("Header: %v", err)
	}
	if len(got) != 1 || string(got[0].Name) != "x" {
		t.Errorf("Header = %v", got)
	}
}

// TestStatusAndTrailerAreReadableFromAnotherGoroutine is the property the
// terminal snapshot exists for. poseidon's own Status and Trailer must be read
// from the goroutine that drove Recv; these must not be. Run with -race.
func TestStatusAndTrailerAreReadableFromAnotherGoroutine(t *testing.T) {
	b := &baseStream{}
	trailer := []conn.HeaderField{{Name: []byte("grpc-status"), Value: []byte("5")}}

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.mu.Lock()
		b.ended, b.termStatus, b.trailer = true, Status{Code: NotFound}, trailer
		b.mu.Unlock()
	}()

	for range 100 {
		_ = b.Status()
		_ = b.Trailer()
		_ = b.Ended()
	}
	<-done

	if got := b.Status().Code; got != NotFound {
		t.Errorf("Status().Code = %v, want NotFound", got)
	}
	if len(b.Trailer()) != 1 {
		t.Errorf("Trailer() = %v", b.Trailer())
	}
}
