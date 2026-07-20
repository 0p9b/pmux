package runtime

import (
	"context"

	domainclient "github.com/0p9b/pmux/internal/domain/client"
	domainconfig "github.com/0p9b/pmux/internal/domain/config"
)

// readOnlyContainerConfig preserves inspection and validation while making the
// container-runtime ownership boundary impossible to bypass through ConfigFile.
type readOnlyContainerConfig struct {
	inner domainconfig.ConfigFile
}

func (c readOnlyContainerConfig) Read(ctx context.Context, path string) (domainconfig.ConfigSnapshot, error) {
	return c.inner.Read(ctx, path)
}
func (c readOnlyContainerConfig) Validate(ctx context.Context, snapshot domainconfig.ConfigSnapshot) []domainconfig.Diagnostic {
	return c.inner.Validate(ctx, snapshot)
}
func (readOnlyContainerConfig) Plan(context.Context, domainconfig.ConfigSnapshot, []domainconfig.PatchOp) (domainconfig.PatchPlan, error) {
	return domainconfig.PatchPlan{}, containerMutationError()
}
func (readOnlyContainerConfig) Apply(context.Context, domainconfig.PatchPlan) (domainconfig.PatchResult, error) {
	return domainconfig.PatchResult{}, containerMutationError()
}

// readOnlyContainerLauncher permits local Claude detection for diagnostics but
// refuses every process or persistent-settings mutation for a container adoption.
type readOnlyContainerLauncher struct {
	inner domainclient.ClientLauncher
}

func (l readOnlyContainerLauncher) Client() domainclient.ClientID { return l.inner.Client() }
func (l readOnlyContainerLauncher) Detect(ctx context.Context) (domainclient.ClientInstall, error) {
	return l.inner.Detect(ctx)
}
func (readOnlyContainerLauncher) Env(domainclient.LaunchSpec) ([]string, error) {
	return nil, containerMutationError()
}
func (readOnlyContainerLauncher) Launch(context.Context, domainclient.LaunchSpec) (domainclient.LaunchResult, error) {
	return domainclient.LaunchResult{}, containerMutationError()
}
func (readOnlyContainerLauncher) PlanPersist(context.Context, domainclient.PersistSpec) (domainclient.PersistPlan, error) {
	return domainclient.PersistPlan{}, containerMutationError()
}
func (readOnlyContainerLauncher) Upsert(context.Context, domainclient.PersistPlan) error {
	return containerMutationError()
}
func (readOnlyContainerLauncher) Unpersist(context.Context) error {
	return containerMutationError()
}
