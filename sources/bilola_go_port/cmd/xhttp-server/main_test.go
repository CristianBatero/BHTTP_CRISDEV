package main

import (
	"net/http"
	"testing"

	"bilola/internal/btun"
)

func TestTargetSelectorRoutesOnlyConfiguredBTUNHost(t *testing.T) {
	selector := makeTargetSelector("127.0.0.1:22", "127.0.0.1:7300",
		"flux.dtmod.shop#f91gs78otu7k.azion.app,btun.example")
	if selector == nil {
		t.Fatal("selector is nil")
	}
	for _, host := range []string{
		"flux.dtmod.shop",
		"FLUX.DTMOD.SHOP:443",
		"f91gs78otu7k.azion.app",
		"F91GS78OTU7K.AZION.APP:443",
		"btun.example",
	} {
		if got := selector(&http.Request{Host: host}); got != "127.0.0.1:7300" {
			t.Fatalf("host %q routed to %q", host, got)
		}
	}
	if got := selector(&http.Request{Host: "legacy.example"}); got != "127.0.0.1:22" {
		t.Fatalf("legacy host routed to %q", got)
	}
}

func TestSharedHostMatcherAcceptsMultipleSeparators(t *testing.T) {
	matcher := makeHostMatcher("f91gs78otu7k.azion.app#shared.example,third.example")
	if matcher == nil {
		t.Fatal("matcher is nil")
	}
	for _, host := range []string{
		"f91gs78otu7k.azion.app",
		"SHARED.EXAMPLE:443",
		"third.example",
	} {
		if !matcher(&http.Request{Host: host}) {
			t.Fatalf("shared host %q was not matched", host)
		}
	}
	if matcher(&http.Request{Host: "legacy.example"}) {
		t.Fatal("legacy host was matched as shared")
	}
}

func TestPayloadTargetSelectorDistinguishesBTUNAndSSH(t *testing.T) {
	selector := makePayloadTargetSelector("127.0.0.1:22", "127.0.0.1:7300")
	btunPayload := append([]byte("cover\r\n"), btun.ClientHello...)
	if got := selector(btunPayload); got != "127.0.0.1:7300" {
		t.Fatalf("BTUN payload routed to %q", got)
	}
	if got := selector([]byte("SSH-2.0-client\r\n")); got != "127.0.0.1:22" {
		t.Fatalf("SSH payload routed to %q", got)
	}
}

func TestTargetSelectorDisabledWithoutTargetOrHosts(t *testing.T) {
	if makeTargetSelector("127.0.0.1:22", "", "flux.dtmod.shop") != nil {
		t.Fatal("selector enabled without BTUN target")
	}
	if makeTargetSelector("127.0.0.1:22", "127.0.0.1:7300", "") != nil {
		t.Fatal("selector enabled without BTUN hosts")
	}
}
