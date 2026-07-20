package discovery

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestDockerEndpointSelectionPolicy(t *testing.T) {
	t.Parallel()
	home := "/Users/tester"
	cases := []struct {
		goos string
		want []dockerEndpoint
	}{
		{"linux", []dockerEndpoint{{Network: "unix", Address: "/var/run/docker.sock"}}},
		{"darwin", []dockerEndpoint{{Network: "unix", Address: "/Users/tester/.docker/run/docker.sock"}, {Network: "unix", Address: "/var/run/docker.sock"}}},
		{"windows", []dockerEndpoint{{Network: "npipe", Address: `\\.\pipe\docker_engine`}}},
	}
	for _, test := range cases {
		got := dockerEndpointCandidates(test.goos, home)
		if len(got) != len(test.want) {
			t.Fatalf("%s endpoints: got %#v want %#v", test.goos, got, test.want)
		}
		for index := range got {
			if got[index] != test.want[index] {
				t.Fatalf("%s endpoint %d: got %#v want %#v", test.goos, index, got[index], test.want[index])
			}
		}
	}
	if got := dockerEndpointCandidates("freebsd", home); got != nil {
		t.Fatalf("unsupported OS selected Docker transport: %#v", got)
	}
}

type errorTransport struct{ err error }

func (t errorTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, t.err }

func TestDockerEndpointAbsenceIsSilentButExistingEndpointErrorsSurface(t *testing.T) {
	t.Parallel()
	missing := DockerSocketEnumerator{
		Client:   &http.Client{Transport: errorTransport{err: os.ErrNotExist}},
		IsAbsent: func(err error) bool { return errors.Is(err, os.ErrNotExist) },
	}
	containers, err := missing.Containers(context.Background())
	if err != nil || len(containers) != 0 {
		t.Fatalf("missing Docker endpoint was not silent: containers=%#v err=%v", containers, err)
	}

	denied := DockerSocketEnumerator{
		Client:   &http.Client{Transport: errorTransport{err: os.ErrPermission}},
		IsAbsent: func(err error) bool { return errors.Is(err, os.ErrNotExist) },
	}
	_, err = denied.Containers(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Docker is unavailable") {
		t.Fatalf("existing/unusable Docker endpoint error was hidden: %v", err)
	}
}
