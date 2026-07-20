package client

import "context"

type ClientID string

const Claude ClientID = "claude"

type ClientInstall struct {
	Path      string `json:"path"`
	Version   string `json:"version"`
	Supported bool   `json:"supported"`
}
type LaunchSpec struct {
	Client     ClientID
	Model      string
	BaseURL    string
	Token      string
	Args       []string
	WorkingDir string
}

// LaunchResult is the terminal status of a client process that started
// successfully. ExitCode is either the client's ordinary status in 0..125 or
// 128 plus the terminating signal number. Signal is empty for ordinary exits.
type LaunchResult struct {
	ExitCode int    `json:"exit_code"`
	Signal   string `json:"signal,omitempty"`
}

type SlotAction string

const (
	SlotUnchanged SlotAction = "unchanged"
	SlotSet       SlotAction = "set"
	SlotUnmanaged SlotAction = "unmanaged"
)

type SlotUpdate struct {
	Action SlotAction
	Model  string
}

type PersistentSlots struct {
	Opus   SlotUpdate
	Sonnet SlotUpdate
	Haiku  SlotUpdate
}

type PersistSpec struct {
	BaseURL string
	Token   string
	Slots   PersistentSlots
}

type PersistPlan struct {
	Path   string
	Before []byte
	After  []byte
	Diff   string
}

type ClientLauncher interface {
	Client() ClientID
	Detect(context.Context) (ClientInstall, error)
	Env(LaunchSpec) ([]string, error)
	Launch(context.Context, LaunchSpec) (LaunchResult, error)
	PlanPersist(context.Context, PersistSpec) (PersistPlan, error)
	Upsert(context.Context, PersistPlan) error
	Unpersist(context.Context) error
}
