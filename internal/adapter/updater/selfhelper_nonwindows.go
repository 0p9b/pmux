//go:build !windows

package updater

import (
	"context"
	"crypto/sha256"
	"errors"
)

type unsupportedSelfHelperOps struct{}

func newPlatformSelfHelperOps() selfHelperOps { return unsupportedSelfHelperOps{} }
func (unsupportedSelfHelperOps) WaitParent(context.Context, int) error { return errors.New("the detached self-update helper is Windows-only") }
func (unsupportedSelfHelperOps) Hash(path string) ([sha256.Size]byte, error) { return fileHash(path) }
func (unsupportedSelfHelperOps) MoveReplace(string, string) error { return errors.New("the detached self-update helper is Windows-only") }
func (unsupportedSelfHelperOps) Remove(path string) error { return errors.New("the detached self-update helper is Windows-only") }
func (unsupportedSelfHelperOps) VerifyVersion(ctx context.Context, path, version string) error { return verifyCommandVersion(ctx, path, version) }
func (unsupportedSelfHelperOps) WriteStatus(path string, status selfUpdateStatus) error { return writeSelfUpdateStatus(path, status) }
func (unsupportedSelfHelperOps) Cleanup(selfUpdatePlan) {}
