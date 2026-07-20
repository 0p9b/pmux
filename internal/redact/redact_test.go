package redact

import "testing"

func TestMaskNeverReturnsCompleteSecret(t *testing.T) {
	for _, secret := range []string{"short", "sk-abcdefghijklmnopqrstuvwxyz012345"} {
		masked := Mask(secret)
		if masked == secret {
			t.Fatalf("Mask(%q) returned complete secret", secret)
		}
	}
}

func TestURLDropsCredentialsQueryAndFragment(t *testing.T) {
	got := URL("https://user:password@example.test/callback?code=secret&state=s#token")
	if got != "https://%3Credacted%3E@example.test/callback" {
		t.Fatalf("URL() = %q", got)
	}
}

func TestMapRedactsSensitiveValues(t *testing.T) {
	got := Map(map[string]string{"host": "127.0.0.1", "api_key": "secret-value"})
	if got["host"] != "127.0.0.1" || got["api_key"] == "secret-value" {
		t.Fatalf("Map() = %#v", got)
	}
}
