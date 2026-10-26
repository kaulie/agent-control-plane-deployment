package main

import "testing"

func TestDefaultListenHostIsAllInterfaces(t *testing.T) {
	t.Setenv("DEPLOYMENT_HOST", "")
	t.Setenv("DEPLOYMENT_HOME", t.TempDir())
	if got := loadConfig().Host; got != "0.0.0.0" {
		t.Fatalf("default listen host = %q, want 0.0.0.0", got)
	}
}

func TestListenHostFromEnv(t *testing.T) {
	t.Setenv("DEPLOYMENT_HOST", "127.0.0.1")
	t.Setenv("DEPLOYMENT_HOME", t.TempDir())
	if got := loadConfig().Host; got != "127.0.0.1" {
		t.Fatalf("listen host = %q, want 127.0.0.1", got)
	}
}

func TestDialableAddrUsesLoopbackForWildcard(t *testing.T) {
	if got := dialableAddr("0.0.0.0:4220", "0.0.0.0", 4220); got != "127.0.0.1:4220" {
		t.Fatalf("wildcard probe = %q", got)
	}
	if got := dialableAddr("[::]:4220", "::", 4220); got != "127.0.0.1:4220" {
		t.Fatalf("ipv6 wildcard probe = %q", got)
	}
	if got := dialableAddr("10.0.0.5:4220", "10.0.0.5", 4220); got != "10.0.0.5:4220" {
		t.Fatalf("specific host probe = %q", got)
	}
}
