package tui

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/0p9b/pmux/internal/redact"
	"github.com/charmbracelet/lipgloss"
)

type StatusKind string

const (
	StatusHealthy StatusKind = "healthy"
	StatusWarning StatusKind = "warning"
	StatusError   StatusKind = "error"
	StatusWorking StatusKind = "working"
	StatusStopped StatusKind = "stopped"
	StatusUnknown StatusKind = "unknown"
)

type Status struct {
	Kind StatusKind
	Text string
}

func (s Status) Label(reducedMotion bool) string {
	text := safeText(s.Text)
	if text == "" {
		text = statusWord(s.Kind)
	}
	marker := statusMarker(s.Kind, reducedMotion)
	return marker + " " + text
}

func statusMarker(kind StatusKind, reducedMotion bool) string {
	switch kind {
	case StatusHealthy:
		return "✓"
	case StatusWarning:
		return "!"
	case StatusError:
		return "×"
	case StatusWorking:
		if reducedMotion {
			return "…"
		}
		return "…"
	case StatusStopped:
		return "○"
	default:
		return "—"
	}
}

func statusWord(kind StatusKind) string {
	switch kind {
	case StatusHealthy:
		return "Healthy"
	case StatusWarning:
		return "Warning"
	case StatusError:
		return "Error"
	case StatusWorking:
		return "Working"
	case StatusStopped:
		return "Stopped"
	default:
		return "Unknown"
	}
}

type palette struct {
	title   lipgloss.Style
	active  lipgloss.Style
	focus   lipgloss.Style
	muted   lipgloss.Style
	error   lipgloss.Style
	warning lipgloss.Style
}

func makePalette(noColor, highContrast, plain bool) palette {
	if noColor || plain {
		base := lipgloss.NewStyle()
		return palette{title: base, active: base, focus: base, muted: base, error: base, warning: base}
	}
	if highContrast {
		return palette{
			title: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("0")),
			active: lipgloss.NewStyle().Bold(true).Underline(true).Foreground(lipgloss.Color("15")),
			focus: lipgloss.NewStyle().Bold(true).Reverse(true),
			muted: lipgloss.NewStyle().Foreground(lipgloss.Color("7")),
			error: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("1")),
			warning: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("11")),
		}
	}
	return palette{
		title: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12")),
		active: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14")),
		focus: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("4")),
		muted: lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
		error: lipgloss.NewStyle().Foreground(lipgloss.Color("9")),
		warning: lipgloss.NewStyle().Foreground(lipgloss.Color("11")),
	}
}

var (
	ansiPattern = regexp.MustCompile(`(?:\x1b\][^\x07]*(?:\x07|\x1b\\))|(?:\x1b\[[0-?]*[ -/]*[@-~])`)
	bearerPattern = regexp.MustCompile(`(?i)Bearer\s+[^\s]+`)
	skPattern = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}\b`)
	assignmentPattern = regexp.MustCompile(`(?i)(ANTHROPIC_AUTH_TOKEN|authorization|x-management-key|access_token|refresh_token|id_token|api[-_]?key|secret[-_]?key|password)\s*[:=]\s*[^\s,;}]+`)
	urlPattern = regexp.MustCompile(`https?://[^\s]+`)
)

func safeText(value string) string {
	value = ansiPattern.ReplaceAllString(value, "")
	value = urlPattern.ReplaceAllStringFunc(value, redact.URL)
	value = bearerPattern.ReplaceAllString(value, "Bearer ********")
	value = skPattern.ReplaceAllStringFunc(value, func(secret string) string { return MaskSecret(secret).String() })
	value = assignmentPattern.ReplaceAllStringFunc(value, func(assignment string) string {
		for i, r := range assignment {
			if r == ':' || r == '=' {
				return strings.TrimSpace(assignment[:i+1]) + " ********"
			}
		}
		return "********"
	})
	value = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
	return value
}

func SafeText(value string) string {
	return safeText(value)
}
