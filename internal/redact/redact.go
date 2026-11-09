// Package redact removes configured secret values from text.
package redact

import "strings"

// Text replaces each non-empty secret with "***".
func Text(s string, secrets []string) string {
	for _, secret := range secrets {
		if secret == "" || len(secret) < 4 {
			continue
		}
		s = strings.ReplaceAll(s, secret, "***")
	}
	return s
}

// Truncate shortens s to at most max bytes without splitting a UTF-8 rune
// when possible. The boolean reports whether bytes were dropped.
func Truncate(s string, max int) (string, bool) {
	if max <= 0 || len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && cut < len(s) && s[cut]&0xC0 == 0x80 {
		cut--
	}
	if cut <= 0 {
		cut = max
	}
	return s[:cut], true
}
