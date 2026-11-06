package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
)

type videoStageStatusError struct{ status int }

func (e videoStageStatusError) Error() string       { return http.StatusText(e.status) }
func (e videoStageStatusError) HTTPStatusCode() int { return e.status }

func TestVideoCreateFailureStageIsFailClosed(t *testing.T) {
	if stage := VideoCreateFailureStage(errors.New("connection reset after write")); stage != VideoStageSubmitted {
		t.Fatalf("transport failure stage = %q", stage)
	}
	if stage := VideoCreateFailureStage(fmt.Errorf("wrapped: %w", videoStageStatusError{status: http.StatusTooManyRequests})); stage != VideoStageCreate {
		t.Fatalf("explicit 429 stage = %q", stage)
	}
	if stage := VideoCreateFailureStage(ErrUnauthorized); stage != VideoStageCreate {
		t.Fatalf("explicit unauthorized stage = %q", stage)
	}
	if stage := VideoCreateFailureStage(videoStageStatusError{status: http.StatusInternalServerError}); stage != VideoStageSubmitted {
		t.Fatalf("explicit 500 stage = %q", stage)
	}
	if stage := VideoCreateFailureStage(fmt.Errorf("Post: %w", context.DeadlineExceeded)); stage != VideoStageSubmitted {
		t.Fatalf("established-connection deadline stage = %q", stage)
	}
}

func TestVideoCreateFailureStageRetriesRequestsNeverSent(t *testing.T) {
	dialTimeout := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("i/o timeout")}
	cases := map[string]error{
		"dial timeout": dialTimeout,
		"socks dial":   fmt.Errorf(`Post "https://grok.com/rest/app-chat/conversations/new": socks connect tcp %s->grok.com:443: %w`, "10.144.144.12:10905", dialTimeout),
		"proxyconnect": fmt.Errorf("proxyconnect tcp: dial tcp 10.144.144.12:10881: i/o timeout"),
		"dns":          &net.DNSError{Err: "no such host", Name: "grok.com"},
	}
	for name, err := range cases {
		if stage := VideoCreateFailureStage(err); stage != VideoStageCreate {
			t.Errorf("%s stage = %q, want %q", name, stage, VideoStageCreate)
		}
	}
}

// A per-dial context deadline aborts the connect attempt before any request
// byte is written, so it must still be retryable. This is the shape a dead
// mesh egress node produces in production: the request never left the process,
// yet the sentinel in the chain is context.DeadlineExceeded.
func TestVideoCreateFailureStageRetriesDialDeadline(t *testing.T) {
	dialDeadline := &net.OpError{Op: "dial", Net: "tcp", Err: context.DeadlineExceeded}
	if stage := VideoCreateFailureStage(dialDeadline); stage != VideoStageCreate {
		t.Fatalf("dial deadline stage = %q, want %q", stage, VideoStageCreate)
	}

	// Production chain for a dead node: the SOCKS5 wrapper reports
	// Op="socks connect" while the aborted TCP connect sits one layer deeper.
	production := fmt.Errorf(
		`Post "https://grok.com/rest/app-chat/conversations/new": socks connect tcp %s->grok.com:443: %w`,
		"10.144.144.12:10900", dialDeadline)
	if stage := VideoCreateFailureStage(production); stage != VideoStageCreate {
		t.Fatalf("socks connect deadline stage = %q, want %q", stage, VideoStageCreate)
	}

	// Same chain wrapped by the adapter's stage annotation: the outer wrapper
	// must not flip the verdict back to submitted.
	staged := WrapVideoStage(VideoStageCreate, 0, production)
	if stage := VideoCreateFailureStage(staged); stage != VideoStageCreate {
		t.Fatalf("annotated dial deadline stage = %q, want %q", stage, VideoStageCreate)
	}
}

// Once the connection is up, a timeout, EOF or reset no longer proves the POST
// was never written — those stay fail-closed.
func TestVideoCreateFailureStageFailsClosedAfterConnect(t *testing.T) {
	cases := map[string]error{
		"eof":               io.EOF,
		"connection reset":  &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")},
		"read deadline":     &net.OpError{Op: "read", Net: "tcp", Err: context.DeadlineExceeded},
		"bare ctx deadline": fmt.Errorf("Post: %w", context.DeadlineExceeded),
		"ctx canceled":      fmt.Errorf("Post: %w", context.Canceled),
	}
	for name, err := range cases {
		if stage := VideoCreateFailureStage(err); stage != VideoStageSubmitted {
			t.Errorf("%s stage = %q, want %q", name, stage, VideoStageSubmitted)
		}
	}
}

// errors.As only surfaces the outermost *net.OpError, so the walker has to
// descend on its own; joined errors must be searched too.
func TestHasDialPhaseOpWalksWholeChain(t *testing.T) {
	nested := &net.OpError{Op: "socks connect", Err: &net.OpError{Op: "dial", Err: errors.New("i/o timeout")}}
	if !hasDialPhaseOp(nested) {
		t.Error("nested dial op not found behind socks connect wrapper")
	}
	if !hasDialPhaseOp(errors.Join(errors.New("cleanup"), &net.OpError{Op: "dial"})) {
		t.Error("dial op inside errors.Join not found")
	}
	if hasDialPhaseOp(&net.OpError{Op: "read", Err: io.EOF}) {
		t.Error("read op reported as dial phase")
	}
	if hasDialPhaseOp(errors.New("plain failure")) {
		t.Error("plain error reported as dial phase")
	}
}
