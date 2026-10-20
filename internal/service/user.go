package service

import (
	"os/user"
	"strings"
)

func currentUserID() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return u.Uid
}

// windowsQuote follows CommandLineToArgvW escaping for a single path argument.
func windowsQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for _, r := range s {
		if r == '\\' {
			slashes++
			continue
		}
		if r == '"' {
			b.WriteString(strings.Repeat("\\", slashes*2+1))
			b.WriteRune(r)
		} else {
			b.WriteString(strings.Repeat("\\", slashes))
			b.WriteRune(r)
		}
		slashes = 0
	}
	b.WriteString(strings.Repeat("\\", slashes*2))
	b.WriteByte('"')
	return b.String()
}

func taskName() string { return "Rhizome Observer " + currentUserID() }
