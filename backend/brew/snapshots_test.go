package brew

import (
	"os"
	"path/filepath"
	"testing"
)

func newTestSnapshotService(t *testing.T) (*SnapshotService, string) {
	t.Helper()
	dir := t.TempDir()
	svc := NewSnapshotService("brew", func() []string { return nil }, func() (string, error) { return dir, nil })
	return svc, dir
}

func TestListSnapshots(t *testing.T) {
	svc, dir := newTestSnapshotService(t)

	writeFile := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("tap x/y\n"), 0644); err != nil {
			t.Fatalf("failed to write fixture %s: %v", name, err)
		}
	}

	writeFile("2026-01-01_10-00-00.Brewfile")
	writeFile("2026-06-15_08-30-00--before-upgrade.Brewfile")
	writeFile("not-a-snapshot.txt")
	writeFile("Brewfile") // no timestamp - should be ignored

	snapshots, err := svc.ListSnapshots()
	if err != nil {
		t.Fatalf("ListSnapshots() error = %v", err)
	}
	if len(snapshots) != 2 {
		t.Fatalf("ListSnapshots() returned %d entries, want 2 (got %+v)", len(snapshots), snapshots)
	}

	// Newest first.
	if snapshots[0].FileName != "2026-06-15_08-30-00--before-upgrade.Brewfile" {
		t.Errorf("snapshots[0].FileName = %q, want the 2026-06-15 snapshot first", snapshots[0].FileName)
	}
	if snapshots[0].Label != "before-upgrade" {
		t.Errorf("snapshots[0].Label = %q, want %q", snapshots[0].Label, "before-upgrade")
	}
	if snapshots[1].Label != "" {
		t.Errorf("snapshots[1].Label = %q, want empty (no label given)", snapshots[1].Label)
	}
}

func TestListSnapshotsMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	svc := NewSnapshotService("brew", func() []string { return nil }, func() (string, error) { return dir, nil })

	snapshots, err := svc.ListSnapshots()
	if err != nil {
		t.Fatalf("ListSnapshots() error = %v, want nil for a missing directory", err)
	}
	if len(snapshots) != 0 {
		t.Errorf("ListSnapshots() = %+v, want empty slice", snapshots)
	}
}

func TestDeleteSnapshot(t *testing.T) {
	svc, dir := newTestSnapshotService(t)
	fileName := "2026-01-01_10-00-00.Brewfile"
	filePath := filepath.Join(dir, fileName)
	if err := os.WriteFile(filePath, []byte("tap x/y\n"), 0644); err != nil {
		t.Fatalf("failed to write fixture: %v", err)
	}

	if err := svc.DeleteSnapshot(fileName); err != nil {
		t.Fatalf("DeleteSnapshot() error = %v", err)
	}
	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		t.Errorf("expected %s to be removed, stat error = %v", filePath, err)
	}
}

func TestRevealSnapshotRejectsMissingFile(t *testing.T) {
	svc, _ := newTestSnapshotService(t)

	// A well-formed but nonexistent snapshot file name - the missing-file
	// check must fire before RevealSnapshot ever shells out to the OS.
	if err := svc.RevealSnapshot("2026-01-01_10-00-00.Brewfile"); err == nil {
		t.Error("RevealSnapshot() succeeded for a nonexistent file, want error")
	}
}

func TestRevealSnapshotRejectsPathTraversal(t *testing.T) {
	svc, _ := newTestSnapshotService(t)

	if err := svc.RevealSnapshot("../outside.Brewfile"); err == nil {
		t.Error("RevealSnapshot() succeeded for a path-traversal name, want rejection")
	}
}

func TestDeleteSnapshotRejectsPathTraversal(t *testing.T) {
	svc, dir := newTestSnapshotService(t)

	// A file that actually exists just outside the snapshots directory - if
	// the traversal guard failed, this would succeed and delete it.
	outsideFile := filepath.Join(filepath.Dir(dir), "outside.Brewfile")
	if err := os.WriteFile(outsideFile, []byte("tap x/y\n"), 0644); err != nil {
		t.Fatalf("failed to write fixture: %v", err)
	}
	defer os.Remove(outsideFile)

	tests := []string{
		"../outside.Brewfile",
		"../../etc/passwd",
		"2026-01-01_10-00-00.txt", // wrong extension, rejected by the pattern
	}

	for _, fileName := range tests {
		if err := svc.DeleteSnapshot(fileName); err == nil {
			t.Errorf("DeleteSnapshot(%q) succeeded, want rejection", fileName)
		}
	}

	if _, err := os.Stat(outsideFile); err != nil {
		t.Errorf("outside file should be untouched, stat error = %v", err)
	}
}
