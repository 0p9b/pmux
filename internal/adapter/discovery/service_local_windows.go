//go:build windows

package discovery

func newLocalServiceEnumerator() ServiceEnumerator {
	return WindowsServiceEnumerator{Source: NativeScheduledTaskSource{}, Limit: defaultScheduledTaskLimit}
}
