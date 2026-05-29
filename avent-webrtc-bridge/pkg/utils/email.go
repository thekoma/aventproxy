package utils

import "strings"

// MaskEmail returns a PII-light representation of an email suitable for logs.
//
// "andrea.cervesato@dontouch.ch" becomes "a***@dontouch.ch". The domain is
// preserved (useful for diagnosing Tuya regional issues) and the local-part is
// reduced to its first character. Inputs that do not look like an email
// (missing @, leading @, trailing @) collapse to "***".
func MaskEmail(email string) string {
	at := strings.Index(email, "@")
	if at <= 0 || at == len(email)-1 {
		return "***"
	}
	return email[:1] + "***" + email[at:]
}
