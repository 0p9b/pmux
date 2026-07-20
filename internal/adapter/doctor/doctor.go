package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	domaindoctor "github.com/0p9b/pmux/internal/domain/doctor"
	"github.com/0p9b/pmux/internal/pmuxerr"
	"github.com/0p9b/pmux/internal/redact"
)

const (
	CheckBinary            = "INS-BINARY"
	CheckCompatibility     = "INS-VERSION"
	CheckConfigParse       = "CFG-PARSE"
	CheckAbsoluteConfig    = "CFG-CWD"
	CheckWSAuth            = "CFG-WSAUTH"
	CheckSafeMode          = "KEY-SAFEMODE"
	CheckPermissions       = "KEY-PERMS"
	CheckServiceDefinition = "SVC-DEFINITION"
	CheckService           = "SVC-STATE"
	CheckPort              = "NET-PORT"
	CheckHealth            = "NET-HEALTH"
	CheckManagementLocal   = "MGMT-LOCAL"
	CheckExposure          = "SEC-EXPOSURE"
	CheckProviders         = "AUTH-STATUS"
	CheckAuthPermissions   = "AUTH-PERMS"
	CheckModels            = "MOD-CATALOG"
	CheckClaude            = "CLI-CLAUDE"
	CheckStateLock         = "STATE-LOCK"
	CheckNoEgress          = "UPDATE-NO-EGRESS"
)

// Source is the read-only boundary used by the built-in checks. Implementations
// may call the platform, service, and management adapters, but checks themselves
// never mutate state or contact a release service.
type Source interface {
	Binary(context.Context) (BinaryFact, error)
	AbsoluteConfig(context.Context) (AbsoluteConfigFact, error)
	SafeMode(context.Context) (SafeModeFact, error)
	Permissions(context.Context) (PermissionsFact, error)
	Service(context.Context) (ServiceFact, error)
	Health(context.Context) (HealthFact, error)
	Providers(context.Context) (ProviderFact, error)
	Models(context.Context) (ModelsFact, error)
	Claude(context.Context) (ClaudeFact, error)
	Compatibility(context.Context) (CompatibilityFact, error)
	UpdateState(context.Context) (UpdateStateFact, error)
	Port(context.Context) (PortFact, error)
	ManagementLocal(context.Context) (ManagementLocalFact, error)
	Exposure(context.Context) (ExposureFact, error)
	StateLock(context.Context) (StateLockFact, error)
}

type BinaryFact struct {
	Path           string
	Exists         bool
	Executable     bool
	ArchitectureOK bool
	Managed        bool
	ChecksumOK     bool
	Version        string
	NotApplicable  string
}

type AbsoluteConfigFact struct {
	ConfigPath                   string
	ConfigReadable               bool
	ConfigParsed                 bool
	ParseDetail                  string
	Managed                      bool
	WSAuthEnabled                bool
	ArgvUsesAbsolutePath         bool
	RuntimeDir                   string
	RuntimeContainsDotEnv        bool
	StoreOverrides               []string
	ProcessContractNotApplicable string
}

type SafeModeFact struct {
	HTTPStatus            int
	Header                string
	PlaceholderConfigured bool
	Authenticated         bool
}

type PermissionTarget struct {
	Path   string
	Auth   bool
	Secure bool
	Detail string
}

type PermissionsFact struct {
	Targets             []PermissionTarget
	SecretNotApplicable string
	AuthNotApplicable   string
}

type ServiceFact struct {
	Backend              string
	Installed            bool
	Required             bool
	Running              bool
	CrashLoop            bool
	DefinitionOwned      bool
	IdentityMatches      bool
	DefinitionUsesConfig bool
	EnvironmentScrubbed  bool
	RuntimeDirClean      bool
	Detail               string
	ExternallyManaged    bool
}

type HealthFact struct {
	HTTPStatus int
	Version    string
	Endpoint   string
	LatencyMS  int64
}

type ProviderFact struct {
	Configured    int
	Usable        int
	Unavailable   int
	Detail        string
	NotApplicable string
}

type ModelsFact struct {
	DiscoverySucceeded bool
	Count              int
	Source             string
	Cause              string
	NotApplicable      string
}

type ClaudeFact struct {
	Found        bool
	VersionKnown bool
	Supported    bool
	Version      string
	Path         string
}

type CompatibilityFact struct {
	VersionKnown        bool
	DetectedVersion     string
	MinimumVersion      string
	FloorSatisfied      bool
	MissingCapabilities []string
}

type UpdateStateFact struct {
	AutomaticChecksEnabled bool
	UnexpectedEgress       bool
	LastCheckExplicit      bool
}

type PortFact struct {
	Host          string
	Port          int
	Listening     bool
	ExpectedOwner bool
	Owner         string
}

type ManagementLocalFact struct {
	Required             bool
	Enabled              bool
	Authenticated        bool
	AllowRemote          bool
	ControlPanelDisabled bool
	NotApplicable        string
}

type ExposureFact struct {
	Host            string
	Loopback        bool
	TLSEnabled      bool
	CertificateOK   bool
	PrivateKeyOK    bool
	RealProxyKey    bool
	Approved        bool
	ManagementLocal bool
}

type StateLockFact struct {
	ReadOnlyStateAccessible bool
	MutationRequested       bool
	MutationLockAvailable   bool
	Holder                  string
}

// Registry preserves registration order. IDs are unique across checks, and at
// most one fix may be registered for a check.
type Registry struct {
	checks        []domaindoctor.Check
	byID          map[string]domaindoctor.Check
	fixes         map[string]domaindoctor.Fix
	prerequisites map[string][]string
}

func NewRegistry() *Registry {
	return &Registry{byID: make(map[string]domaindoctor.Check), fixes: make(map[string]domaindoctor.Fix), prerequisites: make(map[string][]string)}
}

func NewDefaultRegistry(source Source) (*Registry, error) {
	if source == nil {
		return nil, pmuxerr.New(pmuxerr.UnhandledInternal, pmuxerr.Internal, "doctor source is required")
	}
	r := NewRegistry()
	for _, check := range builtins(source) {
		if err := r.RegisterCheck(check); err != nil {
			return nil, err
		}
	}
	for checkID, prerequisites := range defaultPrerequisites() {
		if err := r.RegisterPrerequisites(checkID, prerequisites...); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *Registry) RegisterCheck(check domaindoctor.Check) error {
	if r == nil || check == nil || strings.TrimSpace(check.ID()) == "" {
		return pmuxerr.New(pmuxerr.UnhandledInternal, pmuxerr.Internal, "doctor check must have a non-empty ID")
	}
	if _, exists := r.byID[check.ID()]; exists {
		return pmuxerr.New(pmuxerr.UnhandledInternal, pmuxerr.Internal, "doctor check ID is already registered")
	}
	r.byID[check.ID()] = check
	r.checks = append(r.checks, check)
	return nil
}

func (r *Registry) RegisterFix(fix domaindoctor.Fix) error {
	if r == nil || fix == nil || strings.TrimSpace(fix.ID()) == "" || strings.TrimSpace(fix.CheckID()) == "" {
		return pmuxerr.New(pmuxerr.UnhandledInternal, pmuxerr.Internal, "doctor fix must have non-empty IDs")
	}
	if _, ok := r.byID[fix.CheckID()]; !ok {
		return pmuxerr.New(pmuxerr.UnhandledInternal, pmuxerr.Internal, "doctor fix references an unknown check")
	}
	if _, exists := r.fixes[fix.CheckID()]; exists {
		return pmuxerr.New(pmuxerr.UnhandledInternal, pmuxerr.Internal, "doctor check already has a registered fix")
	}
	r.fixes[fix.CheckID()] = fix
	return nil
}

func (r *Registry) RegisterPrerequisites(checkID string, prerequisites ...string) error {
	if r == nil {
		return pmuxerr.New(pmuxerr.UnhandledInternal, pmuxerr.Internal, "doctor registry is required")
	}
	if _, ok := r.byID[checkID]; !ok {
		return pmuxerr.New(pmuxerr.UnhandledInternal, pmuxerr.Internal, "doctor prerequisite target is not registered")
	}
	previous := append([]string(nil), r.prerequisites[checkID]...)
	seen := make(map[string]bool, len(previous)+len(prerequisites))
	for _, prerequisite := range previous {
		seen[prerequisite] = true
	}
	for _, prerequisite := range prerequisites {
		if prerequisite == checkID {
			return pmuxerr.New(pmuxerr.UnhandledInternal, pmuxerr.Internal, "doctor check cannot depend on itself")
		}
		if _, ok := r.byID[prerequisite]; !ok {
			return pmuxerr.New(pmuxerr.UnhandledInternal, pmuxerr.Internal, "doctor prerequisite is not registered")
		}
		if !seen[prerequisite] {
			seen[prerequisite] = true
			r.prerequisites[checkID] = append(r.prerequisites[checkID], prerequisite)
		}
	}
	if r.hasDependencyCycle() {
		r.prerequisites[checkID] = previous
		return pmuxerr.New(pmuxerr.UnhandledInternal, pmuxerr.Internal, "doctor prerequisite graph contains a cycle")
	}
	return nil
}

func (r *Registry) Prerequisites(checkID string) []string {
	if r == nil {
		return nil
	}
	return append([]string(nil), r.prerequisites[checkID]...)
}

func (r *Registry) hasDependencyCycle() bool {
	visiting, visited := make(map[string]bool), make(map[string]bool)
	var visit func(string) bool
	visit = func(id string) bool {
		if visiting[id] {
			return true
		}
		if visited[id] {
			return false
		}
		visiting[id] = true
		for _, prerequisite := range r.prerequisites[id] {
			if visit(prerequisite) {
				return true
			}
		}
		delete(visiting, id)
		visited[id] = true
		return false
	}
	for id := range r.byID {
		if visit(id) {
			return true
		}
	}
	return false
}

func (r *Registry) Checks() []domaindoctor.Check {
	if r == nil {
		return nil
	}
	return append([]domaindoctor.Check(nil), r.checks...)
}

func (r *Registry) Check(id string) (domaindoctor.Check, bool) { c, ok := r.byID[id]; return c, ok }
func (r *Registry) Fix(checkID string) (domaindoctor.Fix, bool) {
	f, ok := r.fixes[checkID]
	return f, ok
}

// Summary is the exact public doctor summary. Critical counts unresolved failed
// checks whose severity is critical; passing critical checks do not contribute.
type Summary struct {
	Passed   int `json:"passed"`
	Warnings int `json:"warnings"`
	Failed   int `json:"failed"`
	Critical int `json:"critical"`
	ExitCode int `json:"exit_code"`
}

type Report struct {
	Checks  []domaindoctor.CheckResult `json:"checks"`
	Summary Summary                    `json:"summary"`
}

func (r Report) JSON() ([]byte, error) {
	body, err := json.Marshal(r)
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.UnhandledInternal, pmuxerr.Internal, "doctor report could not be encoded")
	}
	return body, nil
}

type Runner struct {
	Registry     *Registry
	KnownSecrets []string
}

func (r Runner) Run(ctx context.Context, ids ...string) (Report, error) {
	if r.Registry == nil {
		return Report{}, pmuxerr.New(pmuxerr.UnhandledInternal, pmuxerr.Internal, "doctor registry is required")
	}
	checks, err := r.selectChecks(ids)
	if err != nil {
		return Report{}, err
	}
	report := Report{Checks: make([]domaindoctor.CheckResult, 0, len(checks))}
	observed := make(map[string]domaindoctor.CheckResult, len(checks))
	for _, check := range checks {
		if err := ctx.Err(); err != nil {
			return Report{}, pmuxerr.Wrap(err, pmuxerr.UnhandledInternal, pmuxerr.Environment, "doctor was interrupted")
		}
		var checkResult domaindoctor.CheckResult
		if prerequisite := failedPrerequisite(r.Registry, observed, check.ID()); prerequisite != "" {
			checkResult = result(domaindoctor.StatusSkip, domaindoctor.SeverityInfo, "Skipped because a prerequisite did not pass", []string{"prerequisite: " + prerequisite}, noRepair())
		} else {
			checkResult = check.Run(ctx)
		}
		checkResult = normalizeResult(check, checkResult, r.KnownSecrets...)
		if _, wired := r.Registry.Fix(check.ID()); !wired {
			checkResult.Repair = noRepair()
		}
		observed[check.ID()] = checkResult
		report.Checks = append(report.Checks, checkResult)
	}
	report.Summary = summarize(report.Checks)
	return report, nil
}

func (r Runner) selectChecks(ids []string) ([]domaindoctor.Check, error) {
	roots := ids
	if len(roots) == 0 {
		checks := r.Registry.Checks()
		roots = make([]string, 0, len(checks))
		for _, check := range checks {
			roots = append(roots, check.ID())
		}
	}
	state := make(map[string]uint8, len(roots))
	selected := make([]domaindoctor.Check, 0, len(roots))
	var include func(string) error
	include = func(id string) error {
		switch state[id] {
		case 1:
			return &pmuxerr.Error{Code: pmuxerr.UnhandledInternal, Class: pmuxerr.Internal, Message: fmt.Sprintf("doctor prerequisite cycle includes %q", id)}
		case 2:
			return nil
		}
		check, ok := r.Registry.Check(id)
		if !ok {
			return &pmuxerr.Error{Code: pmuxerr.ConfigValidationFailed, Class: pmuxerr.User, Message: fmt.Sprintf("unknown doctor check %q", id), Repair: []string{"run pmux doctor without --check to list all checks"}}
		}
		state[id] = 1
		for _, prerequisite := range r.Registry.Prerequisites(id) {
			if err := include(prerequisite); err != nil {
				return err
			}
		}
		state[id] = 2
		selected = append(selected, check)
		return nil
	}
	for _, id := range roots {
		if err := include(id); err != nil {
			return nil, err
		}
	}
	return selected, nil
}

func failedPrerequisite(registry *Registry, observed map[string]domaindoctor.CheckResult, checkID string) string {
	for _, prerequisite := range registry.Prerequisites(checkID) {
		prior, ok := observed[prerequisite]
		if !ok || prior.Status == domaindoctor.StatusFail || prior.Status == domaindoctor.StatusSkip {
			return prerequisite
		}
	}
	return ""
}

func summarize(results []domaindoctor.CheckResult) Summary {
	var out Summary
	for _, result := range results {
		switch result.Status {
		case domaindoctor.StatusPass:
			out.Passed++
		case domaindoctor.StatusWarn:
			out.Warnings++
		case domaindoctor.StatusFail:
			out.Failed++
			if result.Severity == domaindoctor.SeverityCritical {
				out.Critical++
			}
		}
	}
	if out.Failed > 0 {
		out.ExitCode = 7
	}
	return out
}

func normalizeResult(check domaindoctor.Check, result domaindoctor.CheckResult, secrets ...string) domaindoctor.CheckResult {
	result.ID = check.ID()
	switch result.Status {
	case domaindoctor.StatusPass, domaindoctor.StatusWarn, domaindoctor.StatusFail, domaindoctor.StatusSkip:
	default:
		result.Status = domaindoctor.StatusFail
		result.Severity = domaindoctor.SeverityCritical
		result.Summary = "Doctor check returned an invalid status"
		result.Evidence = []string{"invalid status was suppressed"}
		result.Repair = noRepair()
	}
	switch result.Severity {
	case domaindoctor.SeverityInfo, domaindoctor.SeverityWarning, domaindoctor.SeverityCritical:
	default:
		result.Status = domaindoctor.StatusFail
		result.Severity = domaindoctor.SeverityCritical
		result.Summary = "Doctor check returned an invalid severity"
		result.Evidence = []string{"invalid severity was suppressed"}
		result.Repair = noRepair()
	}
	if result.Status == domaindoctor.StatusWarn && result.Severity == domaindoctor.SeverityCritical {
		result.Status = domaindoctor.StatusFail
	}
	if result.Evidence == nil {
		result.Evidence = []string{}
	}
	for i := range result.Evidence {
		result.Evidence[i] = redact.Known(safeText(result.Evidence[i]), secrets...)
	}
	result.Summary = redact.Known(safeText(result.Summary), secrets...)
	result.Repair.Description = redact.Known(safeText(result.Repair.Description), secrets...)
	result.Repair.Verification = redact.Known(safeText(result.Repair.Verification), secrets...)
	return result
}

type checkFunc struct {
	id, title string
	run       func(context.Context) domaindoctor.CheckResult
}

func (c checkFunc) ID() string                                       { return c.id }
func (c checkFunc) Title() string                                    { return c.title }
func (c checkFunc) Run(ctx context.Context) domaindoctor.CheckResult { return c.run(ctx) }

func builtins(source Source) []domaindoctor.Check {
	return []domaindoctor.Check{
		checkFunc{CheckBinary, "CLIProxyAPI binary", func(ctx context.Context) domaindoctor.CheckResult {
			fact, err := source.Binary(ctx)
			return checkBinary(fact, err)
		}},
		checkFunc{CheckCompatibility, "CLIProxyAPI compatibility", func(ctx context.Context) domaindoctor.CheckResult {
			fact, err := source.Compatibility(ctx)
			return checkCompatibility(fact, err)
		}},
		checkFunc{CheckConfigParse, "Configuration syntax", func(ctx context.Context) domaindoctor.CheckResult {
			fact, err := source.AbsoluteConfig(ctx)
			return checkConfigParse(fact, err)
		}},
		checkFunc{CheckAbsoluteConfig, "Absolute config and runtime", func(ctx context.Context) domaindoctor.CheckResult {
			fact, err := source.AbsoluteConfig(ctx)
			return checkAbsoluteConfig(fact, err)
		}},
		checkFunc{CheckWSAuth, "WebSocket authentication", func(ctx context.Context) domaindoctor.CheckResult {
			fact, err := source.AbsoluteConfig(ctx)
			return checkWSAuth(fact, err)
		}},
		checkFunc{CheckSafeMode, "Proxy safe mode", func(ctx context.Context) domaindoctor.CheckResult {
			fact, err := source.SafeMode(ctx)
			return checkSafeMode(fact, err)
		}},
		checkFunc{CheckPermissions, "Private key and state permissions", func(ctx context.Context) domaindoctor.CheckResult {
			fact, err := source.Permissions(ctx)
			return checkPermissions(fact, err)
		}},
		checkFunc{CheckServiceDefinition, "Service definition", func(ctx context.Context) domaindoctor.CheckResult {
			fact, err := source.Service(ctx)
			return checkServiceDefinition(fact, err)
		}},
		checkFunc{CheckService, "Service state", func(ctx context.Context) domaindoctor.CheckResult {
			fact, err := source.Service(ctx)
			return checkService(fact, err)
		}},
		checkFunc{CheckPort, "Proxy port ownership", func(ctx context.Context) domaindoctor.CheckResult {
			fact, err := source.Port(ctx)
			return checkPort(fact, err)
		}},
		checkFunc{CheckHealth, "Proxy health", func(ctx context.Context) domaindoctor.CheckResult {
			fact, err := source.Health(ctx)
			return checkHealth(fact, err)
		}},
		checkFunc{CheckManagementLocal, "Local management API", func(ctx context.Context) domaindoctor.CheckResult {
			fact, err := source.ManagementLocal(ctx)
			return checkManagementLocal(fact, err)
		}},
		checkFunc{CheckExposure, "Proxy network exposure", func(ctx context.Context) domaindoctor.CheckResult {
			fact, err := source.Exposure(ctx)
			return checkExposure(fact, err)
		}},
		checkFunc{CheckProviders, "Provider status", func(ctx context.Context) domaindoctor.CheckResult {
			fact, err := source.Providers(ctx)
			return checkProviders(fact, err)
		}},
		checkFunc{CheckAuthPermissions, "Authentication file permissions", func(ctx context.Context) domaindoctor.CheckResult {
			fact, err := source.Permissions(ctx)
			return checkAuthPermissions(fact, err)
		}},
		checkFunc{CheckModels, "Model catalog", func(ctx context.Context) domaindoctor.CheckResult {
			fact, err := source.Models(ctx)
			return checkModels(fact, err)
		}},
		checkFunc{CheckClaude, "Claude Code", func(ctx context.Context) domaindoctor.CheckResult {
			fact, err := source.Claude(ctx)
			return checkClaude(fact, err)
		}},
		checkFunc{CheckStateLock, "PMux mutation lock", func(ctx context.Context) domaindoctor.CheckResult {
			fact, err := source.StateLock(ctx)
			return checkStateLock(fact, err)
		}},
		checkFunc{CheckNoEgress, "No automatic update egress", func(ctx context.Context) domaindoctor.CheckResult {
			fact, err := source.UpdateState(ctx)
			return checkNoEgress(fact, err)
		}},
	}
}

func defaultPrerequisites() map[string][]string {
	return map[string][]string{
		CheckCompatibility:     {CheckBinary},
		CheckAbsoluteConfig:    {CheckConfigParse},
		CheckWSAuth:            {CheckConfigParse},
		CheckSafeMode:          {CheckConfigParse},
		CheckPermissions:       {CheckConfigParse},
		CheckServiceDefinition: {CheckAbsoluteConfig},
		CheckService:           {CheckServiceDefinition},
		CheckPort:              {CheckService},
		CheckHealth:            {CheckPort},
		CheckManagementLocal:   {CheckConfigParse, CheckHealth},
		CheckExposure:          {CheckConfigParse, CheckManagementLocal},
		CheckProviders:         {CheckManagementLocal},
		CheckAuthPermissions:   {CheckConfigParse},
		CheckModels:            {CheckProviders},
	}
}

func result(status domaindoctor.Status, severity domaindoctor.Severity, summary string, evidence []string, repair domaindoctor.Repair) domaindoctor.CheckResult {
	if evidence == nil {
		evidence = []string{}
	}
	return domaindoctor.CheckResult{Status: status, Severity: severity, Summary: summary, Evidence: evidence, Repair: repair}
}
func noRepair() domaindoctor.Repair { return domaindoctor.Repair{Description: "", Verification: ""} }
func repair(description, verification string, destructive bool) domaindoctor.Repair {
	return domaindoctor.Repair{Available: true, Description: description, Destructive: destructive, ConfirmationRequired: true, Verification: verification}
}
func guidance(description, verification string) domaindoctor.Repair {
	return domaindoctor.Repair{Available: false, Description: description, Destructive: false, ConfirmationRequired: false, Verification: verification}
}
func probeFailure(name string, err error, severity domaindoctor.Severity) domaindoctor.CheckResult {
	return result(domaindoctor.StatusFail, severity, name+" could not be evaluated", []string{"probe returned an error; use --verbose for the wrapped cause"}, noRepair())
}

func checkBinary(f BinaryFact, err error) domaindoctor.CheckResult {
	if err != nil {
		return probeFailure("CLIProxyAPI binary", err, domaindoctor.SeverityCritical)
	}
	if f.NotApplicable != "" {
		return result(domaindoctor.StatusSkip, domaindoctor.SeverityInfo, "Native binary check is not applicable", []string{f.NotApplicable}, noRepair())
	}
	e := []string{"path: " + f.Path}
	if f.Version != "" {
		e = append(e, "version: "+f.Version)
	}
	if !f.Exists {
		return result(domaindoctor.StatusFail, domaindoctor.SeverityCritical, "CLIProxyAPI binary is missing", e, noRepair())
	}
	if !f.Executable {
		return result(domaindoctor.StatusFail, domaindoctor.SeverityCritical, "CLIProxyAPI binary is not executable", e, noRepair())
	}
	if !f.ArchitectureOK {
		return result(domaindoctor.StatusFail, domaindoctor.SeverityCritical, "CLIProxyAPI binary architecture does not match this host", e, noRepair())
	}
	if !filepath.IsAbs(f.Path) {
		return result(domaindoctor.StatusFail, domaindoctor.SeverityCritical, "CLIProxyAPI binary path is not absolute", e, noRepair())
	}
	if f.Managed && !f.ChecksumOK {
		return result(domaindoctor.StatusFail, domaindoctor.SeverityCritical, "Managed CLIProxyAPI binary checksum does not match", e, noRepair())
	}
	return result(domaindoctor.StatusPass, domaindoctor.SeverityCritical, "CLIProxyAPI binary is valid", e, noRepair())
}

func checkConfigParse(f AbsoluteConfigFact, err error) domaindoctor.CheckResult {
	if err != nil {
		return probeFailure("configuration syntax", err, domaindoctor.SeverityCritical)
	}
	evidence := []string{"config: " + f.ConfigPath}
	if f.ParseDetail != "" {
		evidence = append(evidence, f.ParseDetail)
	}
	if !f.ConfigReadable {
		return result(domaindoctor.StatusFail, domaindoctor.SeverityCritical, "Configuration file is not readable", evidence, noRepair())
	}
	if !f.ConfigParsed {
		return result(domaindoctor.StatusFail, domaindoctor.SeverityCritical, "Configuration file is invalid", evidence, noRepair())
	}
	return result(domaindoctor.StatusPass, domaindoctor.SeverityCritical, "Configuration file parses successfully", evidence, noRepair())
}

func checkWSAuth(f AbsoluteConfigFact, err error) domaindoctor.CheckResult {
	if err != nil {
		return probeFailure("WebSocket authentication", err, domaindoctor.SeverityWarning)
	}
	evidence := []string{fmt.Sprintf("ws-auth: %t", f.WSAuthEnabled)}
	if !f.WSAuthEnabled {
		return result(domaindoctor.StatusWarn, domaindoctor.SeverityWarning, "WebSocket authentication is not explicitly enabled", evidence, guidance("enable ws-auth through an explicit config or hardening transaction", "configuration read-back shows ws-auth enabled"))
	}
	return result(domaindoctor.StatusPass, domaindoctor.SeverityWarning, "WebSocket authentication is enabled", evidence, noRepair())
}

func checkAbsoluteConfig(f AbsoluteConfigFact, err error) domaindoctor.CheckResult {
	if err != nil {
		return probeFailure("absolute config", err, domaindoctor.SeverityCritical)
	}
	if f.ProcessContractNotApplicable != "" {
		return result(domaindoctor.StatusSkip, domaindoctor.SeverityInfo, "Native service process contract is not applicable", []string{f.ProcessContractNotApplicable}, noRepair())
	}
	e := []string{"config: " + f.ConfigPath, "runtime: " + f.RuntimeDir}
	if !filepath.IsAbs(f.ConfigPath) || !f.ArgvUsesAbsolutePath {
		return result(domaindoctor.StatusFail, domaindoctor.SeverityCritical, "Service does not use an absolute config path", e, guidance("run the separately confirmed adoption hardening transaction to rewrite the service definition", "service argv and health re-check"))
	}
	if !filepath.IsAbs(f.RuntimeDir) {
		return result(domaindoctor.StatusFail, domaindoctor.SeverityCritical, "Service runtime directory is not absolute", e, guidance("run the separately confirmed adoption hardening transaction to install the canonical runtime directory", "service runtime directory and health re-check"))
	}
	if f.RuntimeContainsDotEnv {
		return result(domaindoctor.StatusFail, domaindoctor.SeverityCritical, "Service runtime directory contains .env", e, guidance("remove the unexpected .env after inspecting it; PMux will not delete an unowned file automatically", "runtime directory inspection and health re-check"))
	}
	if len(f.StoreOverrides) > 0 {
		sort.Strings(f.StoreOverrides)
		e = append(e, "store overrides: "+strings.Join(f.StoreOverrides, ", "))
		return result(domaindoctor.StatusFail, domaindoctor.SeverityCritical, "Service environment can override the recorded config", e, guidance("run the separately confirmed adoption hardening transaction to install a scrubbed service environment", "service environment and health re-check"))
	}
	return result(domaindoctor.StatusPass, domaindoctor.SeverityCritical, "Service uses an absolute config path and clean runtime", e, noRepair())
}

func checkSafeMode(f SafeModeFact, err error) domaindoctor.CheckResult {
	if err != nil {
		return probeFailure("proxy safe mode", err, domaindoctor.SeverityCritical)
	}
	e := []string{fmt.Sprintf("http status: %d", f.HTTPStatus)}
	if f.Header != "" {
		e = append(e, "X-Cpa-Safe-Mode: "+f.Header)
	}
	if f.PlaceholderConfigured || f.Header != "" {
		return result(domaindoctor.StatusFail, domaindoctor.SeverityCritical, "CLIProxyAPI is in safe mode because the configured proxy key is a placeholder", e, repair("back up config, generate a private random proxy key, and remove confirmed placeholders", "authenticated /v1/models returns 200 without X-Cpa-Safe-Mode", true))
	}
	if !f.Authenticated {
		return result(domaindoctor.StatusFail, domaindoctor.SeverityCritical, "Proxy authentication did not succeed", e, noRepair())
	}
	return result(domaindoctor.StatusPass, domaindoctor.SeverityCritical, "Proxy authentication succeeds without safe mode", e, noRepair())
}

func permissionResult(f PermissionsFact, auth bool) domaindoctor.CheckResult {
	targets := make([]PermissionTarget, 0, len(f.Targets))
	if auth && f.AuthNotApplicable != "" {
		return result(domaindoctor.StatusSkip, domaindoctor.SeverityInfo, "Authentication-path permission check is not applicable", []string{f.AuthNotApplicable}, noRepair())
	}
	if !auth && f.SecretNotApplicable != "" {
		return result(domaindoctor.StatusSkip, domaindoctor.SeverityInfo, "Native secret-path permission check is not applicable", []string{f.SecretNotApplicable}, noRepair())
	}
	for _, target := range f.Targets {
		if target.Auth == auth {
			targets = append(targets, target)
		}
	}
	scope := "Secret-bearing"
	if auth {
		scope = "Authentication"
	}
	if len(targets) == 0 {
		return result(domaindoctor.StatusFail, domaindoctor.SeverityCritical, scope+" path permissions or ACLs were not verified", []string{"verified targets: 0"}, noRepair())
	}
	var insecure []string
	for _, target := range targets {
		if !target.Secure {
			insecure = append(insecure, target.Path+": "+target.Detail)
		}
	}
	if len(insecure) > 0 {
		sort.Strings(insecure)
		return result(domaindoctor.StatusFail, domaindoctor.SeverityCritical, scope+" paths are not private", insecure, guidance("repair permissions through the explicit adoption hardening or configuration transaction", "permission or security descriptor read-back"))
	}
	return result(domaindoctor.StatusPass, domaindoctor.SeverityCritical, scope+" paths have private permissions", []string{fmt.Sprintf("verified targets: %d", len(targets))}, noRepair())
}

func checkAuthPermissions(f PermissionsFact, err error) domaindoctor.CheckResult {
	if err != nil {
		return probeFailure("authentication file permissions", err, domaindoctor.SeverityCritical)
	}
	return permissionResult(f, true)
}

func checkPermissions(f PermissionsFact, err error) domaindoctor.CheckResult {
	if err != nil {
		return probeFailure("private permissions", err, domaindoctor.SeverityCritical)
	}
	return permissionResult(f, false)
}

func checkServiceDefinition(f ServiceFact, err error) domaindoctor.CheckResult {
	if err != nil {
		return probeFailure("service definition", err, domaindoctor.SeverityWarning)
	}
	if f.ExternallyManaged {
		return result(domaindoctor.StatusSkip, domaindoctor.SeverityInfo, "Native service definition is externally managed", []string{f.Detail}, noRepair())
	}
	evidence := []string{"backend: " + f.Backend}
	if f.Detail != "" {
		evidence = append(evidence, f.Detail)
	}
	if !f.Required {
		return result(domaindoctor.StatusPass, domaindoctor.SeverityWarning, "Foreground lifecycle does not require a native service definition", evidence, noRepair())
	}
	if !f.Installed {
		return result(domaindoctor.StatusFail, domaindoctor.SeverityWarning, "Selected native service definition is not installed", evidence, noRepair())
	}
	if !f.DefinitionOwned {
		return result(domaindoctor.StatusFail, domaindoctor.SeverityWarning, "Selected service definition is not PMux-owned", evidence, noRepair())
	}
	if !f.IdentityMatches {
		return result(domaindoctor.StatusFail, domaindoctor.SeverityWarning, "Selected service identity is not canonical", evidence, noRepair())
	}
	if !f.DefinitionUsesConfig || !f.EnvironmentScrubbed || !f.RuntimeDirClean {
		return result(domaindoctor.StatusFail, domaindoctor.SeverityWarning, "Selected service definition does not satisfy the managed process contract", evidence, noRepair())
	}
	return result(domaindoctor.StatusPass, domaindoctor.SeverityWarning, "Selected service definition is canonical", evidence, noRepair())
}

func checkService(f ServiceFact, err error) domaindoctor.CheckResult {
	if err != nil {
		return probeFailure("service state", err, domaindoctor.SeverityCritical)
	}
	e := []string{"backend: " + f.Backend}
	if f.ExternallyManaged {
		evidence := []string{"backend: " + f.Backend, f.Detail}
		if !f.Running {
			return result(domaindoctor.StatusFail, domaindoctor.SeverityCritical, "Externally managed CLIProxyAPI container is not running", evidence, noRepair())
		}
		return result(domaindoctor.StatusPass, domaindoctor.SeverityCritical, "Externally managed CLIProxyAPI container is running", evidence, noRepair())
	}
	if f.Detail != "" {
		e = append(e, f.Detail)
	}
	if f.Required && !f.Installed {
		return result(domaindoctor.StatusFail, domaindoctor.SeverityCritical, "Selected service is not installed", e, guidance("install the canonical service with `pmux service install`", "native service status and absolute argv re-check"))
	}
	if f.CrashLoop {
		return result(domaindoctor.StatusFail, domaindoctor.SeverityCritical, "CLIProxyAPI service is crash-looping", e, noRepair())
	}
	if !f.Running {
		return result(domaindoctor.StatusFail, domaindoctor.SeverityCritical, "CLIProxyAPI service is not running", e, guidance("start the selected service with `pmux service start`", "service reaches running and passes health check"))
	}
	return result(domaindoctor.StatusPass, domaindoctor.SeverityCritical, "CLIProxyAPI service is running", e, noRepair())
}
func checkPort(f PortFact, err error) domaindoctor.CheckResult {
	if err != nil {
		return probeFailure("proxy port ownership", err, domaindoctor.SeverityCritical)
	}
	evidence := []string{fmt.Sprintf("listener: %s:%d", valueOr(f.Host, "unknown"), f.Port)}
	if f.Owner != "" {
		evidence = append(evidence, "owner: "+f.Owner)
	}
	if !f.Listening {
		return result(domaindoctor.StatusFail, domaindoctor.SeverityCritical, "Recorded proxy port is not listening", evidence, noRepair())
	}
	if !f.ExpectedOwner {
		return result(domaindoctor.StatusFail, domaindoctor.SeverityCritical, "Recorded proxy port is owned by another process", evidence, noRepair())
	}
	return result(domaindoctor.StatusPass, domaindoctor.SeverityCritical, "Recorded proxy port is owned by the selected CLIProxyAPI process", evidence, noRepair())
}

func checkHealth(f HealthFact, err error) domaindoctor.CheckResult {
	if err != nil {
		return probeFailure("proxy health", err, domaindoctor.SeverityCritical)
	}
	e := []string{fmt.Sprintf("http status: %d", f.HTTPStatus)}
	if f.Endpoint != "" {
		e = append(e, "endpoint: "+f.Endpoint)
	}
	if f.LatencyMS > 0 {
		e = append(e, fmt.Sprintf("latency_ms: %d", f.LatencyMS))
	}
	if f.HTTPStatus != 200 {
		return result(domaindoctor.StatusFail, domaindoctor.SeverityCritical, "CLIProxyAPI health check failed", e, noRepair())
	}
	if f.Version == "" {
		return result(domaindoctor.StatusWarn, domaindoctor.SeverityWarning, "CLIProxyAPI is healthy, but its version is unknown", e, noRepair())
	}
	e = append(e, "version: "+f.Version)
	return result(domaindoctor.StatusPass, domaindoctor.SeverityCritical, "CLIProxyAPI health check passed", e, noRepair())
}
func checkManagementLocal(f ManagementLocalFact, err error) domaindoctor.CheckResult {
	if err != nil {
		return probeFailure("local management API", err, domaindoctor.SeverityCritical)
	}
	if f.NotApplicable != "" {
		return result(domaindoctor.StatusSkip, domaindoctor.SeverityInfo, "Management ownership check is externally managed", []string{f.NotApplicable}, noRepair())
	}
	evidence := []string{fmt.Sprintf("enabled: %t", f.Enabled), fmt.Sprintf("authenticated: %t", f.Authenticated), fmt.Sprintf("allow_remote: %t", f.AllowRemote)}
	if f.Required && !f.Enabled {
		return result(domaindoctor.StatusFail, domaindoctor.SeverityCritical, "Required local management API is not enabled", evidence, noRepair())
	}
	if f.Enabled && !f.Authenticated {
		return result(domaindoctor.StatusFail, domaindoctor.SeverityCritical, "Local management API authentication failed", evidence, noRepair())
	}
	if f.AllowRemote {
		return result(domaindoctor.StatusFail, domaindoctor.SeverityCritical, "Management API allows remote access", evidence, noRepair())
	}
	if f.Enabled && !f.ControlPanelDisabled {
		return result(domaindoctor.StatusWarn, domaindoctor.SeverityWarning, "Management API is local, but the unused control panel is enabled", evidence, noRepair())
	}
	return result(domaindoctor.StatusPass, domaindoctor.SeverityCritical, "Management API is authenticated and localhost-only", evidence, noRepair())
}

func checkExposure(f ExposureFact, err error) domaindoctor.CheckResult {
	if err != nil {
		return probeFailure("proxy network exposure", err, domaindoctor.SeverityCritical)
	}
	evidence := []string{"host: " + valueOr(f.Host, "unknown"), fmt.Sprintf("loopback: %t", f.Loopback), fmt.Sprintf("management_local: %t", f.ManagementLocal)}
	if f.Loopback {
		if !f.ManagementLocal {
			return result(domaindoctor.StatusFail, domaindoctor.SeverityCritical, "Proxy is loopback-bound, but management is remotely exposed", evidence, noRepair())
		}
		return result(domaindoctor.StatusPass, domaindoctor.SeverityCritical, "Proxy and management interfaces are local-only", evidence, noRepair())
	}
	evidence = append(evidence, fmt.Sprintf("tls: %t", f.TLSEnabled), fmt.Sprintf("certificate: %t", f.CertificateOK), fmt.Sprintf("private_key: %t", f.PrivateKeyOK), fmt.Sprintf("real_proxy_key: %t", f.RealProxyKey), fmt.Sprintf("exposure_confirmed: %t", f.Approved))
	if !f.TLSEnabled || !f.CertificateOK || !f.PrivateKeyOK || !f.RealProxyKey || !f.Approved || !f.ManagementLocal {
		return result(domaindoctor.StatusFail, domaindoctor.SeverityCritical, "Non-loopback proxy exposure does not satisfy the explicit security contract", evidence, noRepair())
	}
	return result(domaindoctor.StatusPass, domaindoctor.SeverityCritical, "Explicit proxy exposure satisfies TLS, key, confirmation, and local-management requirements", evidence, noRepair())
}

func checkProviders(f ProviderFact, err error) domaindoctor.CheckResult {
	if err != nil {
		return probeFailure("provider status", err, domaindoctor.SeverityWarning)
	}
	if f.NotApplicable != "" {
		return result(domaindoctor.StatusSkip, domaindoctor.SeverityInfo, "Provider inventory is unavailable for this external container", []string{f.NotApplicable}, noRepair())
	}
	e := []string{fmt.Sprintf("configured: %d", f.Configured), fmt.Sprintf("usable: %d", f.Usable), fmt.Sprintf("unavailable: %d", f.Unavailable)}
	if f.Detail != "" {
		e = append(e, f.Detail)
	}
	if f.Configured == 0 {
		return result(domaindoctor.StatusWarn, domaindoctor.SeverityWarning, "No provider credentials are configured", e, noRepair())
	}
	if f.Usable == 0 {
		return result(domaindoctor.StatusWarn, domaindoctor.SeverityWarning, "No provider credential is currently usable", e, noRepair())
	}
	if f.Unavailable > 0 {
		return result(domaindoctor.StatusWarn, domaindoctor.SeverityWarning, "Some provider credentials are unavailable", e, noRepair())
	}
	return result(domaindoctor.StatusPass, domaindoctor.SeverityWarning, "Provider credentials are usable", e, noRepair())
}

func checkModels(f ModelsFact, err error) domaindoctor.CheckResult {
	if err != nil {
		return probeFailure("model catalog", err, domaindoctor.SeverityWarning)
	}
	if f.NotApplicable != "" {
		return result(domaindoctor.StatusSkip, domaindoctor.SeverityInfo, "Model inventory is unavailable for this external container", []string{f.NotApplicable}, noRepair())
	}
	e := []string{"source: " + f.Source, fmt.Sprintf("models: %d", f.Count)}
	if f.Cause != "" {
		e = append(e, f.Cause)
	}
	if !f.DiscoverySucceeded {
		return result(domaindoctor.StatusWarn, domaindoctor.SeverityWarning, "Live model discovery did not succeed", e, noRepair())
	}
	if f.Count == 0 {
		return result(domaindoctor.StatusWarn, domaindoctor.SeverityWarning, "No models are currently available", e, noRepair())
	}
	return result(domaindoctor.StatusPass, domaindoctor.SeverityWarning, "Live model discovery succeeded", e, noRepair())
}

func checkClaude(f ClaudeFact, err error) domaindoctor.CheckResult {
	if err != nil {
		return probeFailure("Claude Code", err, domaindoctor.SeverityWarning)
	}
	e := []string{}
	if f.Path != "" {
		e = append(e, "path: "+f.Path)
	}
	if f.Version != "" {
		e = append(e, "version: "+f.Version)
	}
	if !f.Found {
		return result(domaindoctor.StatusWarn, domaindoctor.SeverityWarning, "Claude Code was not found", e, noRepair())
	}
	if !f.VersionKnown {
		return result(domaindoctor.StatusWarn, domaindoctor.SeverityWarning, "Claude Code version is unknown", e, noRepair())
	}
	if !f.Supported {
		return result(domaindoctor.StatusWarn, domaindoctor.SeverityWarning, "Claude Code is older than the supported v2 floor", e, noRepair())
	}
	return result(domaindoctor.StatusPass, domaindoctor.SeverityWarning, "Claude Code version is supported", e, noRepair())
}

func checkCompatibility(f CompatibilityFact, err error) domaindoctor.CheckResult {
	if err != nil {
		return probeFailure("CLIProxyAPI compatibility", err, domaindoctor.SeverityCritical)
	}
	e := []string{"detected: " + valueOr(f.DetectedVersion, "unknown"), "minimum: " + valueOr(f.MinimumVersion, "unknown")}
	if !f.VersionKnown {
		return result(domaindoctor.StatusWarn, domaindoctor.SeverityWarning, "CLIProxyAPI version is unknown; feature probes remain required", e, noRepair())
	}
	if !f.FloorSatisfied {
		return result(domaindoctor.StatusFail, domaindoctor.SeverityCritical, "CLIProxyAPI is below PMux's supported management floor", e, noRepair())
	}
	if len(f.MissingCapabilities) > 0 {
		sort.Strings(f.MissingCapabilities)
		e = append(e, "missing: "+strings.Join(f.MissingCapabilities, ", "))
		return result(domaindoctor.StatusFail, domaindoctor.SeverityCritical, "CLIProxyAPI is missing required capabilities", e, noRepair())
	}
	return result(domaindoctor.StatusPass, domaindoctor.SeverityCritical, "CLIProxyAPI compatibility requirements are satisfied", e, noRepair())
}

func checkNoEgress(f UpdateStateFact, err error) domaindoctor.CheckResult {
	if err != nil {
		return probeFailure("automatic update state", err, domaindoctor.SeverityCritical)
	}
	e := []string{fmt.Sprintf("automatic_checks_enabled: %t", f.AutomaticChecksEnabled), fmt.Sprintf("unexpected_egress: %t", f.UnexpectedEgress)}
	if f.AutomaticChecksEnabled || f.UnexpectedEgress {
		return result(domaindoctor.StatusFail, domaindoctor.SeverityCritical, "Automatic update activity or unrequested egress is enabled", e, guidance("disable update checks with `pmux config --scope pmux set update.check false`", "ordinary doctor performs no non-loopback request"))
	}
	if f.LastCheckExplicit {
		e = append(e, "last update check: explicit")
	}
	return result(domaindoctor.StatusPass, domaindoctor.SeverityCritical, "Update checks are user-initiated and no unexpected egress was observed", e, noRepair())
}
func checkStateLock(f StateLockFact, err error) domaindoctor.CheckResult {
	if err != nil {
		return probeFailure("PMux mutation lock", err, domaindoctor.SeverityInfo)
	}
	evidence := []string{fmt.Sprintf("read_only_state_accessible: %t", f.ReadOnlyStateAccessible), fmt.Sprintf("mutation_requested: %t", f.MutationRequested), fmt.Sprintf("mutation_lock_available: %t", f.MutationLockAvailable)}
	if f.Holder != "" {
		evidence = append(evidence, "holder: "+f.Holder)
	}
	if !f.ReadOnlyStateAccessible {
		return result(domaindoctor.StatusFail, domaindoctor.SeverityInfo, "PMux state cannot be inspected read-only", evidence, noRepair())
	}
	if f.MutationRequested && !f.MutationLockAvailable {
		return result(domaindoctor.StatusWarn, domaindoctor.SeverityInfo, "Another PMux mutation currently holds the state lock", evidence, noRepair())
	}
	return result(domaindoctor.StatusPass, domaindoctor.SeverityInfo, "PMux mutation lock is available", evidence, noRepair())
}

func valueOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func safeText(value string) string {
	var out strings.Builder
	out.Grow(len(value))
	for _, r := range value {
		if r == '\n' || r == '\t' || (unicode.IsPrint(r) && r != '\x1b') {
			out.WriteRune(r)
		}
	}
	return out.String()
}
