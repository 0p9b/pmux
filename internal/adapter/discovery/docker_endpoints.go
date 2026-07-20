package discovery

import "path"

type dockerEndpoint struct {
	Network string
	Address string
}

// dockerEndpointCandidates is policy-only so every target's precedence is
// covered without requiring that host OS at test time.
func dockerEndpointCandidates(goos, home string) []dockerEndpoint {
	switch goos {
	case "linux":
		return []dockerEndpoint{{Network: "unix", Address: "/var/run/docker.sock"}}
	case "darwin":
		return []dockerEndpoint{
			{Network: "unix", Address: path.Join(home, ".docker", "run", "docker.sock")},
			{Network: "unix", Address: "/var/run/docker.sock"},
		}
	case "windows":
		return []dockerEndpoint{{Network: "npipe", Address: `\\.\pipe\docker_engine`}}
	default:
		return nil
	}
}
