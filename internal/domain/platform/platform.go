package platform

import "context"

type Platform interface {
	GOOS() string
	ConfigDir() (string, error)
	StateDir() (string, error)
	CacheDir() (string, error)
	DataDir() (string, error)
	OpenBrowser(ctx context.Context, url string) error
	SetClipboard(text string) error
	Shell() string
	IsWSL() bool
	SecurePermissions(path string, isDir bool) error
	VerifySecurePermissions(path string, isDir bool) error
}

type Roots struct {
	Config string `json:"config"`
	State  string `json:"state"`
	Cache  string `json:"cache"`
	Data   string `json:"data"`
}
