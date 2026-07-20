//go:build !linux && !darwin && !windows

package discovery

func newLocalContainerEnumerator() ContainerEnumerator { return nil }
