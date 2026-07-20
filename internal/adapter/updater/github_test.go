package updater

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/0p9b/pmux/internal/domain/update"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestGitHubSourceFetchesMetadataOnlyWhenResolved(t *testing.T) {
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		body := `{"tag_name":"v2.0.0","assets":[` +
			`{"name":"pmux_2.0.0_linux_amd64.tar.gz","browser_download_url":"https://downloads.invalid/pmux"},` +
			`{"name":"checksums.txt","browser_download_url":"https://downloads.invalid/checksums"}]}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}, nil
	})}
	source := NewGitHubSource(client, Target{GOOS: "linux", Arch: "amd64"})
	if requests != 0 {
		t.Fatalf("constructor made %d request(s)", requests)
	}
	release, err := source.Resolve(context.Background(), update.Self, "")
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("Resolve made %d requests, want 1", requests)
	}
	if release.Version != "2.0.0" || release.ArchiveName != "pmux_2.0.0_linux_amd64.tar.gz" {
		t.Fatalf("unexpected release: %+v", release)
	}
}
