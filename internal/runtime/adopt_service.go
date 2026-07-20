package runtime

import (
	"context"

	"github.com/0p9b/pmux/internal/adapter/discovery"
)

// adoptedServiceCutover replaces a specifically discovered foreign lifecycle
// artifact only inside the separately confirmed adoption-hardening transaction.
// The returned closure restores both the exact definition bytes and its prior
// active/inactive state.
type adoptedServiceCutover interface {
	Replace(context.Context, discovery.ServiceEvidence) (func(context.Context) error, error)
}
