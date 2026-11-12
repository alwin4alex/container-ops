// Package logmask hides secret-looking values in log lines before they are
// shown in the browser. It is a safety net, not a guarantee: services should
// not log secrets in the first place.
package logmask

import "regexp"

const masked = "******"

var rules = []struct {
	re   *regexp.Regexp
	repl string
}{
	// KEY=value / key: value where the key looks secret
	{regexp.MustCompile(`(?i)\b([A-Za-z0-9_.-]*(?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credential)[A-Za-z0-9_.-]*)(\s*[=:]\s*)("[^"]*"|'[^']*'|[^\s,;&"']+)`), "${1}${2}" + masked},
	// credentials inside URLs: scheme://user:pass@host
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^\s:/@]+:)[^\s@/]+@`), "${1}" + masked + "@"},
	// Authorization headers
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`), "${1} " + masked},
	// JWTs anywhere
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}`), masked},
}

func Mask(s string) string {
	for _, r := range rules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}
