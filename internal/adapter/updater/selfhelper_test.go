package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
)

type fakeSelfHelperOps struct {
	files       map[string][]byte
	waitErr     error
	failVersion string
	moves       [][2]string
	onMove      func()
	status      selfUpdateStatus
	cleaned     bool
}

func (f *fakeSelfHelperOps) WaitParent(context.Context, int) error { return f.waitErr }
func (f *fakeSelfHelperOps) Hash(path string) ([sha256.Size]byte, error) {
	body, ok := f.files[path]
	if !ok {
		return [sha256.Size]byte{}, errors.New("missing file")
	}
	return sha256.Sum256(body), nil
}
func (f *fakeSelfHelperOps) MoveReplace(source, destination string) error {
	body, ok := f.files[source]
	if !ok {
		return errors.New("missing move source")
	}
	f.moves = append(f.moves, [2]string{source, destination})
	f.files[destination] = append([]byte(nil), body...)
	delete(f.files, source)
	if f.onMove != nil {
		f.onMove()
	}
	return nil
}
func (f *fakeSelfHelperOps) Remove(path string) error { delete(f.files, path); return nil }
func (f *fakeSelfHelperOps) VerifyVersion(_ context.Context, path, version string) error {
	if version == f.failVersion {
		return errors.New("injected postflight failure")
	}
	if _, ok := f.files[path]; !ok {
		return errors.New("verified executable missing")
	}
	return nil
}
func (f *fakeSelfHelperOps) WriteStatus(_ string, status selfUpdateStatus) error {
	f.status = status
	return nil
}
func (f *fakeSelfHelperOps) Cleanup(selfUpdatePlan) { f.cleaned = true }

func TestSelfUpdateHelperSuccess(t *testing.T) {
	plan, ops := helperFixture()
	if err := runSelfUpdateHelper(context.Background(), plan, ops); err != nil {
		t.Fatal(err)
	}
	if string(ops.files[plan.ActivePath]) != "new" || string(ops.files[plan.PreviousPath]) != "old" {
		t.Fatalf("unexpected activation files: %#v", ops.files)
	}
	if ops.status.State != "succeeded" || ops.status.RolledBack || !ops.cleaned {
		t.Fatalf("unexpected helper result: status=%+v cleaned=%v", ops.status, ops.cleaned)
	}
}

func TestSelfUpdateHelperRejectsFingerprintConflict(t *testing.T) {
	plan, ops := helperFixture()
	ops.files[plan.ActivePath] = []byte("changed")
	err := runSelfUpdateHelper(context.Background(), plan, ops)
	if err == nil {
		t.Fatal("fingerprint conflict unexpectedly succeeded")
	}
	if ops.status.State != "conflict" || len(ops.moves) != 0 || string(ops.files[plan.ActivePath]) != "changed" {
		t.Fatalf("conflict mutated executable: status=%+v moves=%v", ops.status, ops.moves)
	}
}

func TestSelfUpdateHelperPostflightFailureRollsBack(t *testing.T) {
	plan, ops := helperFixture()
	ops.failVersion = plan.NextVersion
	err := runSelfUpdateHelper(context.Background(), plan, ops)
	if err == nil {
		t.Fatal("postflight failure unexpectedly succeeded")
	}
	if string(ops.files[plan.ActivePath]) != "old" {
		t.Fatalf("rollback did not restore prior executable: %q", ops.files[plan.ActivePath])
	}
	if ops.status.State != "rolled_back" || !ops.status.RolledBack || ops.status.Version != plan.CurrentVersion {
		t.Fatalf("unexpected rollback status: %+v", ops.status)
	}
}

func TestSelfUpdateHelperInterruptionBeforeActivation(t *testing.T) {
	plan, ops := helperFixture()
	ops.waitErr = context.Canceled
	err := runSelfUpdateHelper(context.Background(), plan, ops)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context cancellation", err)
	}
	if ops.status.State != "interrupted" || len(ops.moves) != 0 || string(ops.files[plan.ActivePath]) != "old" {
		t.Fatalf("interruption mutated executable: status=%+v moves=%v", ops.status, ops.moves)
	}
}

func TestSelfUpdateHelperInterruptionDuringActivationRollsBack(t *testing.T) {
	plan, ops := helperFixture()
	ctx, cancel := context.WithCancel(context.Background())
	ops.onMove = func() {
		ops.onMove = nil
		cancel()
	}
	err := runSelfUpdateHelper(ctx, plan, ops)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context cancellation", err)
	}
	if string(ops.files[plan.ActivePath]) != "old" || ops.status.State != "rolled_back" || !ops.status.RolledBack {
		t.Fatalf("interrupted activation was not rolled back: status=%+v files=%#v", ops.status, ops.files)
	}
}

func TestSelfUpdateHelperInvocationIsPrivateAndExact(t *testing.T) {
	if !IsSelfUpdateHelperInvocation([]string{selfUpdateHelperMarker, "/private/plan.json"}) {
		t.Fatal("exact private helper invocation was not recognized")
	}
	for _, args := range [][]string{
		nil,
		{selfUpdateHelperMarker},
		{selfUpdateHelperMarker, ""},
		{selfUpdateHelperMarker, "/private/plan.json", "--extra"},
		{"update", "self"},
	} {
		if IsSelfUpdateHelperInvocation(args) {
			t.Fatalf("public or malformed argv was recognized as a helper invocation: %q", args)
		}
	}
}

func helperFixture() (selfUpdatePlan, *fakeSelfHelperOps) {
	active := "/private/pmux.exe"
	replacement := "/private/stage/replacement.exe"
	helper := "/private/stage/helper.exe"
	oldBody := []byte("old")
	newBody := []byte("new")
	oldHash := sha256.Sum256(oldBody)
	newHash := sha256.Sum256(newBody)
	plan := selfUpdatePlan{
		ParentPID:         42,
		ActivePath:        active,
		ReplacementPath:   replacement,
		HelperPath:        helper,
		PreviousPath:      active + ".pmux-previous",
		StatusPath:        active + ".pmux-update-status.json",
		ActiveSHA256:      hex.EncodeToString(oldHash[:]),
		ReplacementSHA256: hex.EncodeToString(newHash[:]),
		HelperSHA256:      hex.EncodeToString(newHash[:]),
		CurrentVersion:    "1.0.0",
		NextVersion:       "2.0.0",
	}
	ops := &fakeSelfHelperOps{files: map[string][]byte{
		active:      append([]byte(nil), oldBody...),
		replacement: append([]byte(nil), newBody...),
		helper:      append([]byte(nil), newBody...),
	}}
	return plan, ops
}
