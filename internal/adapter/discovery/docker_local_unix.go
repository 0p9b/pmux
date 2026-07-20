//go:build linux || darwin

package discovery

import (
	"errors"
	"os"
	"runtime"
	"syscall"
)

func newLocalContainerEnumerator() ContainerEnumerator {
	home, _ := os.UserHomeDir()
	for _, endpoint := range dockerEndpointCandidates(runtime.GOOS, home) {
		if endpoint.Network != "unix" {
			continue
		}
		if _, err := os.Stat(endpoint.Address); err != nil {
			if unixDockerEndpointAbsent(err) {
				continue
			}
			return unavailableContainerEnumerator{cause: err}
		}
		return DockerSocketEnumerator{SocketPath: endpoint.Address, IsAbsent: unixDockerEndpointAbsent}
	}
	return nil
}

func unixDockerEndpointAbsent(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOENT)
}
