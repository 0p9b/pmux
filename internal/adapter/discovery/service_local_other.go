//go:build !windows

package discovery

func newLocalServiceEnumerator() ServiceEnumerator {
	return LocalServiceEnumerator{}
}
