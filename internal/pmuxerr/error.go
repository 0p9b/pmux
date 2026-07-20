package pmuxerr

import (
	"errors"
	"fmt"
)

type Class string

const (
	User        Class = "user"
	Environment Class = "environment"
	Upstream    Class = "upstream"
	Internal    Class = "internal"
)

const (
	CodeOK                = "ok"
	CodeInternal          = "internal_error"
	CodeUsage             = "usage_error"
	CodeConfig            = "config_error"
	CodeDependencyMissing = "dependency_missing"
	CodeAuth              = "auth_error"
	CodeNetwork           = "network_error"
	CodeUnhealthy         = "unhealthy"
	CodeOwnershipConflict = "ownership_conflict"
	CodeCanceled          = "canceled"
	CodeLaunchFailed      = "launch_failed"
	CodeNotExecutable     = "not_executable"
	CodeExecutableMissing = "executable_not_found"
	CodeInterrupted       = "interrupted"
)

// Condition codes are stable, never-reused identifiers from the specification.
// Outcome codes above remain accepted for command-boundary and Cobra usage errors.
const (
	InstallDownloadFailed      = "PMUX-1001"
	InstallIntegrityFailed     = "PMUX-1002"
	InstallUnsupportedTarget   = "PMUX-1003"
	InstallRollbackAttempted   = "PMUX-1004"
	InstallReleaseLookupFailed = "PMUX-1005"
	ConfigUnreadable           = "PMUX-2001"
	ConfigValidationFailed     = "PMUX-2002"
	ConfigPathMismatch         = "PMUX-2003"
	ConfigInsecurePermissions  = "PMUX-2004"
	ConfigSafeMode             = "PMUX-2005"
	ConfigMutationConflict     = "PMUX-2006"
	ServiceStartFailed         = "PMUX-3001"
	ServiceForeignOwner        = "PMUX-3002"
	ServiceBackendUnavailable  = "PMUX-3003"
	ServiceHealthDeadline      = "PMUX-3004"
	ManagementUnreachable      = "PMUX-4001"
	ManagementAuthRejected     = "PMUX-4002"
	ProviderUnreachable        = "PMUX-4003"
	OAuthCallbackPortConflict  = "PMUX-4004"
	OAuthTimeout               = "PMUX-5001"
	OAuthStateMismatch         = "PMUX-5002"
	OAuthNoUsableCredential    = "PMUX-5003"
	AuthFileInvalid            = "PMUX-5004"
	ClientBinaryMissing        = "PMUX-6001"
	ClientLaunchFailed         = "PMUX-6002"
	ClientSettingsConflict     = "PMUX-6003"
	JournalCorrupt             = "PMUX-7001"
	UnhandledUpstreamShape     = "PMUX-7002"
	UnhandledInternal          = "PMUX-7099"
)

type Error struct {
	Code        string   `json:"code"`
	Class       Class    `json:"class,omitempty"`
	Message     string   `json:"message"`
	Explanation string   `json:"explanation,omitempty"`
	Evidence    []string `json:"evidence,omitempty"`
	Repair      []string `json:"repair,omitempty"`
	DocsURL     string   `json:"docs_url,omitempty"`
	Cause       error    `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return e.Code
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func New(code string, class Class, message string) *Error {
	return &Error{Code: code, Class: class, Message: message}
}

func Wrap(err error, code string, class Class, message string) *Error {
	if err == nil {
		return nil
	}
	var existing *Error
	if errors.As(err, &existing) && code == "" {
		return existing
	}
	return &Error{Code: code, Class: class, Message: message, Cause: err}
}

func Capability(feature, required, detected string) *Error {
	if detected == "" {
		detected = "unknown"
	}
	message := fmt.Sprintf("This feature requires CLIProxyAPI ≥ %s (detected: %s). Run `pmux update proxy` to upgrade.", required, detected)
	return &Error{Code: CodeDependencyMissing, Class: Upstream, Message: message, Explanation: feature + " requires CLIProxyAPI ≥ " + required}
}

func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var pe *Error
	if !errors.As(err, &pe) {
		return 1
	}
	if code, ok := conditionExitCodes[pe.Code]; ok {
		return code
	}
	switch pe.Code {
	case CodeUsage:
		return 2
	case CodeConfig:
		return 3
	case CodeDependencyMissing:
		return 4
	case CodeAuth:
		return 5
	case CodeNetwork:
		return 6
	case CodeUnhealthy:
		return 7
	case CodeOwnershipConflict:
		return 9
	case CodeCanceled:
		return 10
	case CodeLaunchFailed:
		return 125
	case CodeNotExecutable:
		return 126
	case CodeExecutableMissing:
		return 127
	case CodeInterrupted:
		return 130
	default:
		return 1
	}
}

var conditionExitCodes = map[string]int{
	InstallDownloadFailed:      6,
	InstallIntegrityFailed:     7,
	InstallUnsupportedTarget:   4,
	InstallRollbackAttempted:   7,
	InstallReleaseLookupFailed: 6,
	ConfigUnreadable:           3,
	ConfigValidationFailed:     3,
	ConfigPathMismatch:         3,
	ConfigInsecurePermissions:  3,
	ConfigSafeMode:             7,
	ConfigMutationConflict:     9,
	ServiceStartFailed:         7,
	ServiceForeignOwner:        9,
	ServiceBackendUnavailable:  4,
	ServiceHealthDeadline:      7,
	ManagementUnreachable:      6,
	ManagementAuthRejected:     5,
	ProviderUnreachable:        6,
	OAuthCallbackPortConflict:  6,
	OAuthTimeout:               5,
	OAuthStateMismatch:         5,
	OAuthNoUsableCredential:    5,
	AuthFileInvalid:            5,
	ClientBinaryMissing:        127,
	ClientLaunchFailed:         125,
	ClientSettingsConflict:     9,
	JournalCorrupt:             1,
	UnhandledUpstreamShape:     4,
	UnhandledInternal:          1,
}
