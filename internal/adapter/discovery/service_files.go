package discovery

import (
	"bufio"
	"context"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/0p9b/pmux/internal/domain/service"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

// LocalServiceEnumerator observes native definition files. It does not invoke
// a service manager and therefore cannot start, stop, enable, or reload one.
type LocalServiceEnumerator struct {
	Paths []string
	GOOS  string
}

func (e LocalServiceEnumerator) Services(ctx context.Context) ([]ServiceEvidence, error) {
	goos := e.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	paths := append([]string(nil), e.Paths...)
	if len(paths) == 0 {
		home, _ := os.UserHomeDir()
		switch goos {
		case "linux":
			paths = []string{filepath.Join(home, ".config", "systemd", "user"), "/etc/systemd/user", "/etc/systemd/system"}
		case "darwin":
			paths = []string{
				filepath.Join(home, "Library", "LaunchAgents"),
				"/Library/LaunchAgents",
				"/System/Library/LaunchAgents",
			}
		default:
			return nil, nil
		}
	}
	var results []ServiceEvidence
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, pmuxerr.Wrap(err, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "service discovery was canceled")
		}
		info, err := os.Stat(path)
		if os.IsNotExist(err) || os.IsPermission(err) {
			continue
		}
		if err != nil {
			return nil, pmuxerr.Wrap(err, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "could not inspect a service definition path")
		}
		if info.Mode().IsRegular() {
			if serviceEvidence, ok := readServiceDefinition(path, goos); ok {
				results = append(results, serviceEvidence)
			}
			continue
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			lower := strings.ToLower(entry.Name())
			if goos == "linux" && !strings.HasSuffix(lower, ".service") {
				continue
			}
			if goos == "darwin" && !strings.HasSuffix(lower, ".plist") {
				continue
			}
			definition := filepath.Join(path, entry.Name())
			if serviceEvidence, ok := readServiceDefinition(definition, goos); ok {
				results = append(results, serviceEvidence)
			}
		}
	}
	return results, nil
}

func readServiceDefinition(path, goos string) (ServiceEvidence, bool) {
	file, err := os.Open(path)
	if err != nil {
		return ServiceEvidence{}, false
	}
	defer file.Close()
	if goos == "darwin" {
		return readLaunchd(path, file)
	}
	return readSystemd(path, file)
}

func readSystemd(path string, input io.Reader) (ServiceEvidence, bool) {
	evidence := ServiceEvidence{Backend: service.BackendSystemdUser, Identity: filepath.Base(path), Definition: path}
	scanner := bufio.NewScanner(input)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "ExecStart":
			evidence.Argv = splitQuoted(strings.TrimSpace(value))
			if len(evidence.Argv) > 0 {
				evidence.Executable = evidence.Argv[0]
			}
		case "WorkingDirectory":
			evidence.WorkingDir = strings.TrimSpace(value)
		}
	}
	if !looksLikeCore(evidence.Executable, evidence.Argv) && !strings.Contains(strings.ToLower(evidence.Identity), "cliproxy") {
		return ServiceEvidence{}, false
	}
	evidence.ConfigPath, _ = configFromArgv(evidence.Argv, evidence.WorkingDir)
	evidence.PMuxOwned = strings.HasPrefix(evidence.Identity, "pmux-cliproxyapi@")
	return evidence, true
}

func readLaunchd(path string, input io.Reader) (ServiceEvidence, bool) {
	decoder := xml.NewDecoder(input)
	var currentKey string
	var inArguments bool
	evidence := ServiceEvidence{Backend: service.BackendLaunchd, Identity: strings.TrimSuffix(filepath.Base(path), ".plist"), Definition: path}
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		switch value := token.(type) {
		case xml.StartElement:
			if value.Name.Local == "array" && currentKey == "ProgramArguments" {
				inArguments = true
			}
			if value.Name.Local != "key" && value.Name.Local != "string" {
				continue
			}
			var text string
			if decoder.DecodeElement(&text, &value) != nil {
				continue
			}
			if value.Name.Local == "key" {
				currentKey = text
				continue
			}
			if inArguments {
				evidence.Argv = append(evidence.Argv, text)
			} else {
				switch currentKey {
				case "Label":
					evidence.Identity = text
				case "WorkingDirectory":
					evidence.WorkingDir = text
				}
			}
		case xml.EndElement:
			if value.Name.Local == "array" {
				inArguments = false
				currentKey = ""
			}
		}
	}
	if len(evidence.Argv) > 0 {
		evidence.Executable = evidence.Argv[0]
	}
	if !looksLikeCore(evidence.Executable, evidence.Argv) && !strings.Contains(strings.ToLower(evidence.Identity), "cliproxy") {
		return ServiceEvidence{}, false
	}
	evidence.ConfigPath, _ = configFromArgv(evidence.Argv, evidence.WorkingDir)
	evidence.PMuxOwned = strings.HasPrefix(evidence.Identity, "dev.pmux.cliproxyapi.")
	return evidence, true
}

func splitQuoted(input string) []string {
	var values []string
	var current strings.Builder
	var quote rune
	escaped := false
	flush := func() {
		if current.Len() > 0 {
			values = append(values, current.String())
			current.Reset()
		}
	}
	for _, char := range input {
		if escaped {
			current.WriteRune(char)
			escaped = false
			continue
		}
		if char == '\\' && quote != 0 {
			escaped = true
			continue
		}
		if quote != 0 {
			if char == quote {
				quote = 0
			} else {
				current.WriteRune(char)
			}
			continue
		}
		if char == '\'' || char == '"' {
			quote = char
			continue
		}
		if char == ' ' || char == '\t' {
			flush()
			continue
		}
		current.WriteRune(char)
	}
	if escaped {
		current.WriteRune('\\')
	}
	flush()
	return values
}
