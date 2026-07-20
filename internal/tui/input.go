package tui

import (
	"context"
	"io"
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
)

type inputChoice struct {
	value  string
	label  string
	prompt string
	secret bool
}

type inputPrompt struct {
	action   ActionID
	label    string
	value    string
	secret   bool
	optional bool
	prefix   []string
	choices  []inputChoice
	next     *inputPrompt
	response *io.PipeWriter
	selected int
}

func inputFor(id ActionID, selected []string) (inputPrompt, bool) {
	switch id {
	case ActionConfigSet:
		if len(selected) == 0 { return inputPrompt{}, false }
		return inputPrompt{action: id, label: "New value for " + selected[0], prefix: append([]string(nil), selected...)}, true
	case ActionSettingsSet:
		if len(selected) == 0 { return inputPrompt{}, false }
		return inputPrompt{action: id, label: "New setting value for " + selected[0], prefix: append([]string(nil), selected...)}, true
	case ActionConfigRestore:
		return inputPrompt{action: id, label: "Proxy backup ID"}, true
	case ActionSettingsRestore:
		return inputPrompt{action: id, label: "PMux backup ID"}, true
	case ActionLogsExport:
		return inputPrompt{action: id, label: "Private export path"}, true
	case ActionLaunchPersist:
		return inputPrompt{action: id, label: "Persistent slot assignment (opus|sonnet|haiku=<model|unmanaged>)"}, true
	default:
		return inputPrompt{}, false
	}
}

func (m *Shell) providerLoginInput() (inputPrompt, bool) {
	rows := m.filteredProviders()
	if len(rows) == 0 {
		return inputPrompt{}, false
	}
	row := rows[min(m.selected[Providers], len(rows)-1)]
	choices := make([]inputChoice, 0, len(row.Flows))
	for _, flow := range row.Flows {
		switch flow {
		case "browser":
			choices = append(choices, inputChoice{value: flow, label: "Browser callback"})
		case "paste_callback":
			choices = append(choices, inputChoice{value: flow, label: "Paste callback URL (headless)"})
		case "device_code":
			choices = append(choices, inputChoice{value: flow, label: "Device authorization"})
		case "api_key":
			choices = append(choices, inputChoice{value: flow, label: "API key", prompt: "Protected API key for " + row.Name, secret: true})
		case "vertex_import":
			choices = append(choices, inputChoice{value: flow, label: "Vertex service-account import", prompt: "Vertex service-account JSON path"})
		}
	}
	if len(choices) == 0 {
		return inputPrompt{}, false
	}
	if len(choices) == 1 {
		if next, needsValue := providerFlowInput(row.ID, choices[0]); needsValue {
			return next, true
		}
	}
	return inputPrompt{
		action:  ActionProviderLogin,
		label:   "Authentication method for " + row.Name,
		prefix:  []string{row.ID},
		choices: choices,
	}, true
}

func providerFlowInput(providerID string, choice inputChoice) (inputPrompt, bool) {
	prefix := []string{providerID, choice.value}
	if choice.prompt == "" {
		return inputPrompt{action: ActionProviderLogin, prefix: prefix}, false
	}
	input := inputPrompt{action: ActionProviderLogin, label: choice.prompt, prefix: prefix, secret: choice.secret}
	if choice.value == "vertex_import" {
		input.next = &inputPrompt{
			action:   ActionProviderLogin,
			label:    "Optional Vertex credential prefix",
			optional: true,
		}
	}
	return input, true
}

func (m *Shell) updateInput(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.input == nil {
		return m, nil
	}
	if len(m.input.choices) > 0 {
		switch key.Type {
		case tea.KeyEsc, tea.KeyCtrlC:
			m.input = nil
			m.lastErr = "Canceled; no changes were made."
			return m, nil
		case tea.KeyUp:
			m.input.selected = (m.input.selected + len(m.input.choices) - 1) % len(m.input.choices)
			return m, nil
		case tea.KeyDown, tea.KeyTab:
			m.input.selected = (m.input.selected + 1) % len(m.input.choices)
			return m, nil
		case tea.KeyEnter:
			choice := m.input.choices[m.input.selected]
			providerID := m.input.prefix[0]
			next, needsValue := providerFlowInput(providerID, choice)
			if needsValue {
				m.input = &next
				m.lastErr = ""
				return m, nil
			}
			meta, ok := Action(ActionProviderLogin)
			m.input = nil
			if !ok {
				m.lastErr = "This action has no canonical CLI equivalent."
				return m, nil
			}
			return m.confirmOrExecute(meta, next.prefix, nil)
		}
		return m, nil
	}
	switch key.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		if m.input.response != nil {
			_ = m.input.response.CloseWithError(context.Canceled)
			m.protectedWriter = nil
			if m.cancel != nil {
				m.cancel()
			}
		}
		m.input.value = ""
		m.input = nil
		m.lastErr = "Canceled; no changes were made."
		return m, nil
	case tea.KeyBackspace, tea.KeyDelete:
		runes := []rune(m.input.value)
		if len(runes) > 0 {
			m.input.value = string(runes[:len(runes)-1])
		}
		return m, nil
	case tea.KeyEnter:
		value := m.input.value
		if strings.TrimSpace(value) == "" && !m.input.optional {
			m.lastErr = "A value is required; press Esc to cancel."
			return m, nil
		}
		if m.input.response != nil {
			response := m.input.response
			payload := []byte(value)
			m.input.value = ""
			m.input = nil
			m.lastErr = ""
			return m, func() tea.Msg {
				_, err := response.Write(payload)
				clearSecret(payload)
				closeErr := response.Close()
				if err == nil {
					err = closeErr
				}
				return protectedInputSubmittedMsg{err: err}
			}
		}
		arguments := append([]string(nil), m.input.prefix...)
		if !m.input.secret {
			arguments = append(arguments, value)
		}
		if m.input.next != nil {
			next := *m.input.next
			next.prefix = arguments
			m.input.value = ""
			m.input = &next
			m.lastErr = ""
			return m, nil
		}
		meta, ok := Action(m.input.action)
		if !ok {
			m.input.value = ""
			m.input = nil
			m.lastErr = "This action has no canonical CLI equivalent."
			return m, nil
		}
		var secret []byte
		if m.input.secret {
			secret = []byte(value)
		}
		m.input.value = ""
		m.input = nil
		m.lastErr = ""
		return m.confirmOrExecute(meta, arguments, secret)
	}
	for _, character := range key.Runes {
		if unicode.IsPrint(character) {
			m.input.value += string(character)
		}
	}
	return m, nil
}

func (m *Shell) inputView() string {
	if m.input == nil {
		return ""
	}
	if len(m.input.choices) > 0 {
		var lines []string
		lines = append(lines, safeText(m.input.label)+":")
		for index, choice := range m.input.choices {
			marker := "  "
			if index == m.input.selected {
				marker = "> "
			}
			lines = append(lines, marker+safeText(choice.label))
		}
		lines = append(lines, "Up/Down selects; Enter continues; Esc cancels")
		return strings.Join(lines, "\n")
	}
	value := safeText(m.input.value)
	if m.input.secret {
		value = strings.Repeat("*", len([]rune(m.input.value)))
	}
	return safeText(m.input.label) + ": " + value + "_  (Enter continues; Esc cancels)"
}

func clearSecret(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
