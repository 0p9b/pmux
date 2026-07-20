//go:build !linux && !darwin && !windows

package discovery

import "context"

// LocalProcessEnumerator is intentionally conservative on unsupported
// platforms. Discovery still consumes explicit, service, and listener
// observations.
type LocalProcessEnumerator struct{}

func (LocalProcessEnumerator) Processes(context.Context) ([]ProcessEvidence, error) {
	return nil, nil
}
