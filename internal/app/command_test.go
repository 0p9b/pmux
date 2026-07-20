package app

import (
	"context"
	"errors"
	"testing"

	"github.com/0p9b/pmux/internal/pmuxerr"
)

func TestUnavailableUseCasesFailsClosed(t *testing.T) {
	_, err := (UnavailableUseCases{}).Execute(context.Background(), Invocation{Operation: "models.list"}, nil)
	if err == nil { t.Fatal("unavailable composition reported success") }
	var typed *pmuxerr.Error
	if !errors.As(err, &typed) { t.Fatalf("error type = %T", err) }
	if typed.Code != pmuxerr.CodeDependencyMissing || typed.Class != pmuxerr.Environment { t.Fatalf("error = %+v", typed) }
	if pmuxerr.ExitCode(err) != 4 { t.Fatalf("exit = %d", pmuxerr.ExitCode(err)) }
}

func TestUseCaseFuncPreservesInvocationAndSink(t *testing.T) {
	want := Invocation{Operation: "providers.list", JSON: true}
	called := false
	fn := UseCaseFunc(func(_ context.Context, got Invocation, sink EventSink) (Result, error) {
		called = true
		if got.Operation != want.Operation || got.JSON != want.JSON { t.Fatalf("invocation = %#v", got) }
		if sink == nil { t.Fatal("event sink is nil") }
		return Result{Data: "ok"}, nil
	})
	result, err := fn.Execute(context.Background(), want, func(Event) error { return nil })
	if err != nil || !called || result.Data != "ok" { t.Fatalf("called=%v result=%#v err=%v", called, result, err) }
}
