//go:build windows

package windowstask

func enrichCOMError(err error) error {
	return comFailure(err)
}
