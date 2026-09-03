package health

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeChecker struct {
	name string
	err  error
}

func (f fakeChecker) Name() string                  { return f.name }
func (f fakeChecker) Check(_ context.Context) error { return f.err }

func TestRegistry_NotReadyBeforeMarkReady(t *testing.T) {
	r := NewRegistry()
	if err := r.Ready(t.Context()); err == nil {
		t.Error("expected Ready() to error before MarkReady is called")
	}
}

func TestRegistry_ReadyAfterMarkReady(t *testing.T) {
	r := NewRegistry()
	r.MarkReady()
	if err := r.Ready(t.Context()); err != nil {
		t.Errorf("expected Ready() to succeed after MarkReady, got: %v", err)
	}
}

func TestRegistry_NotReadyAfterMarkNotReady(t *testing.T) {
	r := NewRegistry()
	r.MarkReady()
	r.MarkNotReady()
	if err := r.Ready(t.Context()); err == nil {
		t.Error("expected Ready() to error after MarkNotReady")
	}
}

func TestRegistry_FailingCheckerMakesNotReady(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeChecker{name: "database", err: errors.New("connection refused")})
	r.MarkReady()

	err := r.Ready(t.Context())
	if err == nil {
		t.Fatal("expected Ready() to error when a checker fails")
	}
	if !strings.Contains(err.Error(), "database") {
		t.Errorf("expected error to name the failing checker, got: %v", err)
	}
}

func TestRegistry_PassingCheckerKeepsReady(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeChecker{name: "database", err: nil})
	r.MarkReady()

	if err := r.Ready(t.Context()); err != nil {
		t.Errorf("expected Ready() to succeed when all checkers pass, got: %v", err)
	}
}

type deadlineRecordingChecker struct {
	hadDeadline *bool
}

func (c deadlineRecordingChecker) Name() string { return "deadline-check" }
func (c deadlineRecordingChecker) Check(ctx context.Context) error {
	_, ok := ctx.Deadline()
	*c.hadDeadline = ok
	return nil
}

func TestRegistry_Ready_BoundsEachCheckerWithATimeout(t *testing.T) {
	r := NewRegistry()
	r.MarkReady()
	var hadDeadline bool
	r.Register(deadlineRecordingChecker{hadDeadline: &hadDeadline})

	if err := r.Ready(t.Context()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hadDeadline {
		t.Error("expected Check to be called with a context bounded by a per-checker timeout")
	}
}

func TestRegistry_Live_AlwaysSucceeds(t *testing.T) {
	r := NewRegistry()
	if err := r.Live(t.Context()); err != nil {
		t.Errorf("expected Live() to always succeed, got: %v", err)
	}
	r.Register(fakeChecker{name: "database", err: errors.New("down")})
	if err := r.Live(t.Context()); err != nil {
		t.Errorf("expected Live() to ignore checker state, got: %v", err)
	}
}
