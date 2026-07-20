package doctor

import "context"

type Status string
const ( StatusPass Status = "pass"; StatusWarn Status = "warn"; StatusFail Status = "fail"; StatusSkip Status = "skip" )
type Severity string
const ( SeverityInfo Severity = "info"; SeverityWarning Severity = "warning"; SeverityCritical Severity = "critical" )

type Repair struct { Available bool `json:"available"`; Description string `json:"description"`; Destructive bool `json:"destructive"`; ConfirmationRequired bool `json:"confirmation_required"`; Verification string `json:"verification"` }
type CheckResult struct { ID string `json:"id"`; Status Status `json:"status"`; Severity Severity `json:"severity"`; Summary string `json:"summary"`; Evidence []string `json:"evidence"`; Repair Repair `json:"repair"` }
type FixResult struct { CheckID string `json:"check_id"`; Changed bool `json:"changed"`; Verified bool `json:"verified"`; Summary string `json:"summary"` }
type Check interface { ID() string; Title() string; Run(context.Context) CheckResult }
type Fix interface { ID() string; CheckID() string; Apply(context.Context, bool) (FixResult, error) }
