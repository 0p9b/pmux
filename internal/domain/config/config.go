package config

import "context"

type Config struct {
	Host            string
	Port            int
	AuthDir         string
	WSAuth          bool
	APIKeys         []string
	ManagementLocal bool
}

type PatchOp struct {
	Path  string
	Value any
	Unset bool
}

type ConfigSnapshot struct {
	Path        string
	Fingerprint [32]byte
	Config      Config
}

type PatchPlan struct {
	Snapshot        ConfigSnapshot
	Operations      []PatchOp
	Rendered        []byte
	RestartRequired bool
	Diff            string
}

type PatchResult struct {
	BackupPath      string
	Fingerprint     [32]byte
	RestartRequired bool
}

type Diagnostic struct { ID string `json:"id"`; Severity string `json:"severity"`; Message string `json:"message"` }

type ConfigFile interface {
	Read(context.Context, string) (ConfigSnapshot, error)
	Plan(context.Context, ConfigSnapshot, []PatchOp) (PatchPlan, error)
	Apply(context.Context, PatchPlan) (PatchResult, error)
	Validate(context.Context, ConfigSnapshot) []Diagnostic
}
