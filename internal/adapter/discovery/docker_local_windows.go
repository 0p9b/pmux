//go:build windows

package discovery

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

func newLocalContainerEnumerator() ContainerEnumerator {
	endpoint := dockerEndpointCandidates("windows", "")[0]
	return DockerSocketEnumerator{
		Client:   newWindowsDockerHTTPClient(endpoint.Address),
		IsAbsent: windowsDockerEndpointAbsent,
	}
}

func newWindowsDockerHTTPClient(pipe string) *http.Client {
	transport := &http.Transport{
		DisableCompression: true,
		MaxIdleConns:       1,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialWindowsNamedPipe(ctx, pipe)
		},
	}
	return &http.Client{Transport: transport, Timeout: 2 * time.Second}
}

func dialWindowsNamedPipe(ctx context.Context, name string) (net.Conn, error) {
	path, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	for {
		handle, openErr := windows.CreateFile(
			path,
			windows.GENERIC_READ|windows.GENERIC_WRITE,
			0,
			nil,
			windows.OPEN_EXISTING,
			windows.FILE_FLAG_OVERLAPPED,
			0,
		)
		if openErr == nil {
			file := os.NewFile(uintptr(handle), name)
			if file == nil {
				_ = windows.CloseHandle(handle)
				return nil, windows.ERROR_INVALID_HANDLE
			}
			return &windowsNamedPipeConn{File: file, name: name}, nil
		}
		if !errors.Is(openErr, windows.ERROR_PIPE_BUSY) {
			return nil, openErr
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func windowsDockerEndpointAbsent(err error) bool {
	return errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}

type windowsNamedPipeConn struct {
	*os.File
	name string
}

func (c *windowsNamedPipeConn) LocalAddr() net.Addr  { return windowsPipeAddr(c.name) }
func (c *windowsNamedPipeConn) RemoteAddr() net.Addr { return windowsPipeAddr(c.name) }

type windowsPipeAddr string

func (windowsPipeAddr) Network() string { return "npipe" }
func (address windowsPipeAddr) String() string { return string(address) }
