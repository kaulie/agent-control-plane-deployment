package main

import "testing"

func TestPackageFromGitRequiresURL(t *testing.T) {
	_, err := packageFromGit(t.TempDir(), "web-cursor", "", "main", 60)
	if err == nil {
		t.Fatal("expected error for empty gitRepoUrl")
	}
}

func TestPackageFromGitRequiresServiceID(t *testing.T) {
	_, err := packageFromGit(t.TempDir(), "", "https://example.com/repo.git", "main", 60)
	if err == nil {
		t.Fatal("expected error for empty serviceId")
	}
}
