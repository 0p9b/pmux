package version

import "testing"

func TestCurrentLDFlagsTakePrecedence(t *testing.T) {
	oldVersion, oldCommit, oldDate := Version, Commit, Date
	t.Cleanup(func() { Version, Commit, Date = oldVersion, oldCommit, oldDate })
	Version, Commit, Date = "v1.2.3", "abc", "2026-07-20T00:00:00Z"
	got := Current()
	if got.Version != Version || got.Commit != Commit || got.Date != Date {
		t.Fatalf("Current() = %#v", got)
	}
}

func TestCurrentDevelopmentVersion(t *testing.T) {
	oldVersion, oldCommit, oldDate := Version, Commit, Date
	t.Cleanup(func() { Version, Commit, Date = oldVersion, oldCommit, oldDate })
	Version, Commit, Date = "", "", ""
	got := Current()
	if got.Version == "" {
		t.Fatal("Current().Version is empty")
	}
}
