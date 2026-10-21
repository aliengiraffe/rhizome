package config

import (
	"strings"
	"testing"
)

func TestGatewayConfiguration(t *testing.T) {
	p := fixture(t, "gateway.mode='lan'\ngateway.listen='127.0.0.1:7947'\ngateway.advertise='127.0.0.1:7948'\n")
	c, err := Resolve(map[string]string{"config": p, "gateway-listen": "127.0.0.1:7950"}, func(k string) string {
		if k == "RHIZOME_GATEWAY_LISTEN" {
			return "127.0.0.1:7949"
		}
		if k == "RHIZOME_GATEWAY_ADVERTISE" {
			return "127.0.0.1:7951"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Gateway.Mode != "lan" || c.Sources["gateway.mode"] != "file" || c.Gateway.Listen != "127.0.0.1:7950" || c.Sources["gateway.listen"] != "flag" || c.Gateway.Advertise != "127.0.0.1:7951" || c.Sources["gateway.advertise"] != "env" {
		t.Fatalf("bad resolution: %+v", c)
	}
	for _, body := range []string{"gateway.mode='public'", "gateway.listen='127.0.0.1:7947'", "gateway.mode='lan'\ngateway.listen='0.0.0.0:7947'", "gateway.mode='lan'\ngateway.listen='127.0.0.1:0'", "gateway.token='secret'", "Gateway.mode='lan'"} {
		_, err := Resolve(map[string]string{"config": fixture(t, body)}, noenv)
		if err == nil {
			t.Errorf("accepted %s", strings.Split(body, "\n")[0])
		}
	}
}
