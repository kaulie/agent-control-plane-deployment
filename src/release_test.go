package main

import "testing"

func TestPackageFromGitRequiresURL(t *testing.T) {
	_, err := packageFromGit(t.TempDir(), "", "main", 60)
	if err == nil {
		t.Fatal("expected error for empty gitRepoUrl")
	}
}
