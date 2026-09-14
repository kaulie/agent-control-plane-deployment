package main

import (
	"testing"
	"time"
)

func TestPackageFromGitRequiresURL(t *testing.T) {
	_, err := packageFromGit("web-cursor", "", "main", 60, nil, nil)
	if err == nil {
		t.Fatal("expected error for empty gitRepoUrl")
	}
}

func TestPackageFromGitRequiresServiceID(t *testing.T) {
	_, err := packageFromGit("", "https://example.com/repo.git", "main", 60, nil, nil)
	if err == nil {
		t.Fatal("expected error for empty serviceId")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{-1, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{20 * 1024 * 1024, "20.0 MB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	if got := humanDuration(3400 * time.Millisecond); got != "3.4s" {
		t.Errorf("humanDuration = %q, want %q", got, "3.4s")
	}
}
