package redact

import (
	"net/url"
	"strings"
)

var sensitiveKeys = map[string]struct{}{
	"authorization": {}, "x-management-key": {}, "api-key": {}, "api_key": {},
	"token": {}, "access_token": {}, "refresh_token": {}, "client_secret": {},
	"private_key": {}, "anthropic_auth_token": {}, "password": {}, "secret": {},
}

func IsSensitiveKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	if _, ok := sensitiveKeys[key]; ok {
		return true
	}
	return strings.HasSuffix(key, "_key") || strings.HasSuffix(key, "-key") ||
		strings.HasSuffix(key, "_token") || strings.HasSuffix(key, "-token") ||
		strings.HasSuffix(key, "_secret") || strings.HasSuffix(key, "-secret")
}

func Mask(value string) string {
	if value == "" {
		return ""
	}
	if len(value) <= 11 {
		return "********"
	}
	return value[:7] + "…" + value[len(value)-4:]
}

func URL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<redacted-url>"
	}
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	if u.User != nil {
		u.User = url.User("<redacted>")
	}
	return u.String()
}

func Known(text string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "<redacted>")
		}
	}
	return text
}

func Map(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for key, value := range values {
		if IsSensitiveKey(key) {
			out[key] = Mask(value)
		} else {
			out[key] = value
		}
	}
	return out
}
