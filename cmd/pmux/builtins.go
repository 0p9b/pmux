package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/0p9b/pmux/internal/app"
	"github.com/0p9b/pmux/internal/pmuxerr"
	"github.com/spf13/cobra"
)

type versionReport struct {
	PMuxVersion       string `json:"pmux_version"`
	Commit            string `json:"commit,omitempty"`
	Date              string `json:"date,omitempty"`
	OS                string `json:"os"`
	Arch              string `json:"arch"`
	ConfigRoot        string `json:"config_root"`
	CLIProxyAPI       string `json:"cliproxyapi_version"`
	CLIProxyAPISource string `json:"cliproxyapi_version_source"`
}

func completionCommand(d dispatcher) *cobra.Command {
	cmd := &cobra.Command{
		Use:       "completion <bash|zsh|fish|powershell>",
		Short:     "Write a shell completion script",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		var err error
		switch args[0] {
		case "bash":
			err = cmd.Root().GenBashCompletion(d.deps.Out)
		case "zsh":
			err = cmd.Root().GenZshCompletion(d.deps.Out)
		case "fish":
			err = cmd.Root().GenFishCompletion(d.deps.Out, true)
		case "powershell":
			err = cmd.Root().GenPowerShellCompletion(d.deps.Out)
		default:
			return usage("completion shell must be bash, zsh, fish, or powershell")
		}
		return builtinOutputError(err)
	}
	return cmd
}

func versionCommand(d dispatcher) *cobra.Command {
	var short bool
	cmd := &cobra.Command{Use: "version", Short: "Show PMux and recorded core versions", Args: cobra.NoArgs}
	cmd.Flags().BoolVar(&short, "short", false, "print only the PMux version")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		info := d.deps.Version()
		if short {
			if d.flags.JSON {
				return builtinOutputError(renderResult(d.deps.Out, app.Result{Data: map[string]string{"version": info.Version}}, true))
			}
			_, err := fmt.Fprintln(d.deps.Out, info.Version)
			return builtinOutputError(err)
		}
		configRoot, err := resolveConfigRoot(d.deps, d.flags.ConfigDir)
		if err != nil {
			return err
		}
		report := versionReport{
			PMuxVersion: info.Version, Commit: info.Commit, Date: info.Date,
			OS: d.deps.GOOS, Arch: d.deps.GOARCH, ConfigRoot: configRoot,
			CLIProxyAPI: "unknown", CLIProxyAPISource: "unavailable",
		}
		human := []string{
			"PMux " + report.PMuxVersion,
			"Platform: " + report.OS + "/" + report.Arch,
			"Config root: " + report.ConfigRoot,
			"CLIProxyAPI: unknown (no safe recorded or detected version)",
		}
		if report.Commit != "" {
			human = append(human, "Commit: "+report.Commit)
		}
		if report.Date != "" {
			human = append(human, "Built: "+report.Date)
		}
		return builtinOutputError(renderResult(d.deps.Out, app.Result{Data: report, Human: human}, d.flags.JSON))
	}
	return cmd
}

func resolveConfigRoot(deps dependencies, override string) (string, error) {
	if override != "" {
		absolute, err := filepath.Abs(override)
		if err != nil {
			return "", configRootError(err)
		}
		return filepath.Clean(absolute), nil
	}
	home, err := deps.UserHome()
	if err != nil {
		return "", configRootError(err)
	}
	switch deps.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "PMux"), nil
	case "windows":
		root := deps.Getenv("APPDATA")
		if root == "" {
			return "", configRootError(fmt.Errorf("APPDATA is not set"))
		}
		return strings.TrimRight(root, `\/`) + `\PMux`, nil
	default:
		root := deps.Getenv("XDG_CONFIG_HOME")
		if root == "" {
			root = filepath.Join(home, ".config")
		}
		return filepath.Join(root, "pmux"), nil
	}
}

func configRootError(cause error) error {
	err := pmuxerr.Wrap(cause, pmuxerr.ConfigPathMismatch, pmuxerr.Environment, "PMux could not resolve the config root.")
	err.Repair = []string{"Set the platform config-root environment variable or pass --config-dir with an accessible absolute path."}
	return err
}

func builtinOutputError(cause error) error {
	if cause == nil {
		return nil
	}
	return pmuxerr.Wrap(cause, pmuxerr.UnhandledInternal, pmuxerr.Internal, "PMux could not write command output.")
}
