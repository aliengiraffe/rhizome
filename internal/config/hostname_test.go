package config

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestHostnameConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct {
		value, want string
		invalid     bool
	}{
		{"", "", false}, {"   ", "", false}, {"  workstation  ", "workstation", false},
		{strings.Repeat("界", 21) + "a", strings.Repeat("界", 21) + "a", false},
		{strings.Repeat("界", 22), "", true}, {"bad\nlabel", "", true}, {"bad\x7flabel", "", true},
	} {
		t.Run(fmt.Sprintf("%q", tc.value), func(t *testing.T) {
			p := fixture(t, fmt.Sprintf("hostname = %q\n", tc.value))
			c, err := Resolve(map[string]string{"config": p}, noenv)
			if tc.invalid {
				if err == nil || !strings.Contains(err.Error(), "hostname") {
					t.Fatalf("got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.Hostname != tc.want || c.Sources["hostname"] != "file" {
				t.Fatalf("hostname %q, source %q", c.Hostname, c.Sources["hostname"])
			}
			var out bytes.Buffer
			if err := c.Write(&out); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), fmt.Sprintf("hostname = %q # source: file", tc.want)) {
				t.Fatal(out.String())
			}
		})
	}
	c, err := Resolve(nil, noenv)
	if err != nil || c.Hostname != "" || c.Sources["hostname"] != "default" {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := NormalizeHostname(string([]byte{0xff})); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
}
