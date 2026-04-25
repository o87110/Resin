package main

import (
	"strings"
	"testing"
)

func TestSocksSupportLabel(t *testing.T) {
	if got := socksSupportLabel(false, ""); got != "HTTP + SOCKS5" {
		t.Fatalf("default label = %q, want %q", got, "HTTP + SOCKS5")
	}
	if got := socksSupportLabel(true, "tok"); got != "HTTP + SOCKS5" {
		t.Fatalf("protected label = %q, want %q", got, "HTTP + SOCKS5")
	}
	if got := socksSupportLabel(true, ""); got != "HTTP + SOCKS5 + SOCKS4 (unsafe)" {
		t.Fatalf("unsafe label = %q, want %q", got, "HTTP + SOCKS5 + SOCKS4 (unsafe)")
	}
}

func TestInsecureSOCKS4StartupWarning(t *testing.T) {
	if msg := insecureSOCKS4StartupWarning(false, ""); msg != "" {
		t.Fatalf("expected empty warning, got %q", msg)
	}
	if msg := insecureSOCKS4StartupWarning(true, ""); msg != "" {
		t.Fatalf("expected empty warning without proxy token, got %q", msg)
	}
	msg := insecureSOCKS4StartupWarning(true, "tok")
	if msg == "" {
		t.Fatal("expected warning when SOCKS4 is enabled with proxy token configured")
	}
	if !strings.Contains(msg, "RESIN_ALLOW_INSECURE_SOCKS4=true") {
		t.Fatalf("warning should mention RESIN_ALLOW_INSECURE_SOCKS4=true, got %q", msg)
	}
}
