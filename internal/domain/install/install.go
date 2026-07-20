package install

import "context"

type Target struct {
	OS   string
	Arch string
}
type Release struct {
	Version      string
	AssetName    string
	AssetURL     string
	ChecksumsURL string
}
type DownloadedAsset struct {
	Path   string
	Name   string
	SHA256 [32]byte
}
type ExtractedBinary struct {
	Path    string
	Version string
}
type Installation struct {
	ID         string `json:"id"`
	Mode       string `json:"mode"`
	BinaryPath string `json:"binary_path"`
	ConfigPath string `json:"config_path"`
	AuthDir    string `json:"auth_dir"`
	Version    string `json:"version"`
	Container  bool   `json:"container"`
}
type Installer interface {
	Resolve(context.Context, string) (Release, error)
	Download(context.Context, Release, string) (DownloadedAsset, error)
	VerifyArchive(context.Context, DownloadedAsset, []byte) error
	Extract(context.Context, DownloadedAsset, string) (ExtractedBinary, error)
	VerifyExecutable(context.Context, ExtractedBinary, Target) error
	Install(context.Context, ExtractedBinary) error
	Rollback(context.Context) error
}
