//go:build windows

package discovery

import (
	"net/http"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWindowsDockerTransportIsBoundedNativePipe(t *testing.T) {
	t.Parallel()
	enumerator, ok := newLocalContainerEnumerator().(DockerSocketEnumerator)
	if !ok { t.Fatalf("unexpected Windows Docker enumerator: %T", newLocalContainerEnumerator()) }
	if enumerator.Client == nil || enumerator.Client.Timeout != 2*time.Second { t.Fatalf("Windows Docker client is not bounded: %#v", enumerator.Client) }
	transport, ok := enumerator.Client.Transport.(*http.Transport)
	if !ok || transport.DialContext == nil { t.Fatalf("Windows Docker client has no native dialer: %#v", enumerator.Client.Transport) }
	if !enumerator.IsAbsent(windows.ERROR_FILE_NOT_FOUND) || !enumerator.IsAbsent(windows.ERROR_PATH_NOT_FOUND) || enumerator.IsAbsent(windows.ERROR_ACCESS_DENIED) {
		t.Fatal("Windows named-pipe absence classification is incorrect")
	}
}
