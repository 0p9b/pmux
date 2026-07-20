package provider

import (
	"reflect"
	"testing"
)

func TestRegistryContainsAllMVPProviders(t *testing.T) {
	got := map[string]bool{}
	for _, provider := range Registry() {
		got[string(provider.ID)] = true
	}
	for _, id := range []string{"codex", "codex-compatible", "claude", "claude-compatible", "antigravity", "kimi", "xai", "gemini", "interactions", "vertex", "openrouter", "openai-compatible"} {
		if !got[id] {
			t.Errorf("provider %q missing", id)
		}
	}
}

func TestSubprocessFlagsAreClosedAndExact(t *testing.T) {
	got := map[string][]string{}
	for _, provider := range Registry() {
		for flow, flags := range provider.SubprocessFlags {
			got[string(provider.ID)+"/"+string(flow)] = flags
		}
	}
	want := map[string][]string{
		"codex/browser": {"-codex-login"},
		"codex/device_code": {"-codex-device-login"},
		"claude/browser": {"-claude-login"},
		"antigravity/browser": {"-antigravity-login"},
		"kimi/device_code": {"-kimi-login"},
		"xai/device_code": {"-xai-login"},
		"vertex/vertex_import": {"-vertex-import"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("subprocess map = %#v, want %#v", got, want)
	}
}
