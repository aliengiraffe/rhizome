package service

import (
	"encoding/xml"
	"io"
	"rhizome/internal/channel"
	"strings"
	"testing"
)

func TestNativeDefinitions(t *testing.T) {
	for _, platform := range []string{"darwin", "linux", "windows"} {
		t.Run(platform, func(t *testing.T) {
			m := manager{platform: platform, configPath: `/home/test/a b&c/service.json`, definition: "/service"}
			c := Config{Binary: `/opt/a b/rhizome`, Observer: channel.ObserverOptions{Runtime: "/runtime"}}
			definition := m.render(c)
			if !strings.Contains(definition, "service") || !strings.Contains(definition, "run") {
				t.Fatal(definition)
			}
			if platform != "linux" {
				d := xml.NewDecoder(strings.NewReader(definition))
				for {
					_, err := d.Token()
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, action := range []string{"install", "start", "stop", "status", "uninstall"} {
				if len(m.commands(action)) == 0 {
					t.Fatal(action)
				}
			}
			if platform == "windows" && (!strings.Contains(definition, "InteractiveToken") || !strings.Contains(definition, "IgnoreNew")) {
				t.Fatal("not user scoped")
			}
			if platform == "linux" && !strings.Contains(definition, `ExecStart="/opt/a b/rhizome"`) {
				t.Fatal("unquoted binary")
			}
		})
	}
}
func TestEscaping(t *testing.T) {
	if got := unitArg(`/a%u/"x`); got != `"/a%%u/\"x"` {
		t.Fatal(got)
	}
	if got := windowsQuote(`C:\a b\`); got != `"C:\a b\\"` {
		t.Fatal(got)
	}
}
