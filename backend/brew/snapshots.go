package brew

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"WailBrew/backend/system"
)

// SnapshotEntry describes one saved Brewfile snapshot.
type SnapshotEntry struct {
	FileName  string `json:"fileName"`
	Label     string `json:"label"`
	CreatedAt string `json:"createdAt"` // RFC3339
	Size      int64  `json:"size"`
}

// SnapshotService manages Brewfile snapshots: point-in-time backups of the
// installed formulae, casks, and taps that can later be restored via
// `brew bundle install`.
type SnapshotService struct {
	brewPath        string
	getBrewEnvFunc  func() []string
	getSnapshotsDir func() (string, error)
}

// NewSnapshotService creates a new snapshot service.
func NewSnapshotService(
	brewPath string,
	getBrewEnvFunc func() []string,
	getSnapshotsDir func() (string, error),
) *SnapshotService {
	return &SnapshotService{
		brewPath:        brewPath,
		getBrewEnvFunc:  getBrewEnvFunc,
		getSnapshotsDir: getSnapshotsDir,
	}
}

// snapshotTimestampLayout is used both to name new snapshot files and to
// parse the timestamp back out of existing ones.
const snapshotTimestampLayout = "2006-01-02_15-04-05"

// snapshotFileNamePattern matches files created by CreateSnapshot:
// "<timestamp>.Brewfile" or "<timestamp>--<label>.Brewfile". It is also used
// to reject anything else when resolving a snapshot path, so arbitrary file
// names (e.g. "../../etc/passwd") can never be read, restored, or deleted.
var snapshotFileNamePattern = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2})(?:--(.+))?\.Brewfile$`)

// snapshotLabelSanitizer strips anything from a user-provided label that
// isn't safe in a filename, collapsing runs of unsafe characters to a single
// hyphen.
var snapshotLabelSanitizer = regexp.MustCompile(`[^a-zA-Z0-9._]+`)

// CreateSnapshot dumps the currently installed formulae, casks, and taps into
// a new timestamped Brewfile under the snapshots directory. label is
// optional and, when given, is sanitized and appended to the filename so the
// snapshot is easier to recognize later.
func (s *SnapshotService) CreateSnapshot(label string) (SnapshotEntry, error) {
	dir, err := s.getSnapshotsDir()
	if err != nil {
		return SnapshotEntry{}, fmt.Errorf("failed to resolve snapshots directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return SnapshotEntry{}, fmt.Errorf("failed to create snapshots directory: %w", err)
	}

	timestamp := time.Now().Format(snapshotTimestampLayout)
	cleanLabel := strings.Trim(snapshotLabelSanitizer.ReplaceAllString(strings.TrimSpace(label), "-"), "-")

	fileName := timestamp + ".Brewfile"
	if cleanLabel != "" {
		fileName = timestamp + "--" + cleanLabel + ".Brewfile"
	}
	filePath := filepath.Join(dir, fileName)

	cmd := exec.Command(s.brewPath, "bundle", "dump", "--file="+filePath, "--force")
	system.ApplyEnvironment(cmd, s.getBrewEnvFunc())
	output, err := cmd.CombinedOutput()
	if err != nil {
		return SnapshotEntry{}, fmt.Errorf("brew bundle dump failed: %v\nOutput: %s", err, string(output))
	}

	info, err := os.Stat(filePath)
	if err != nil {
		return SnapshotEntry{}, fmt.Errorf("snapshot created but could not be read back: %w", err)
	}

	return SnapshotEntry{
		FileName:  fileName,
		Label:     cleanLabel,
		CreatedAt: info.ModTime().UTC().Format(time.RFC3339),
		Size:      info.Size(),
	}, nil
}

// ListSnapshots returns all saved snapshots, newest first.
func (s *SnapshotService) ListSnapshots() ([]SnapshotEntry, error) {
	dir, err := s.getSnapshotsDir()
	if err != nil {
		return nil, fmt.Errorf("failed to resolve snapshots directory: %w", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []SnapshotEntry{}, nil
		}
		return nil, fmt.Errorf("failed to read snapshots directory: %w", err)
	}

	snapshots := make([]SnapshotEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		match := snapshotFileNamePattern.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}

		createdAt, err := time.ParseInLocation(snapshotTimestampLayout, match[1], time.Local)
		if err != nil {
			createdAt = info.ModTime()
		}

		snapshots = append(snapshots, SnapshotEntry{
			FileName:  entry.Name(),
			Label:     match[2],
			CreatedAt: createdAt.UTC().Format(time.RFC3339),
			Size:      info.Size(),
		})
	}

	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].CreatedAt > snapshots[j].CreatedAt
	})

	return snapshots, nil
}

// RestoreSnapshot installs everything listed in the given snapshot via
// `brew bundle install`. cleanup additionally removes any installed
// formulae, casks, or taps that are not listed in the snapshot, matching the
// machine to it exactly.
func (s *SnapshotService) RestoreSnapshot(fileName string, cleanup bool) error {
	filePath, err := s.resolveSnapshotPath(fileName)
	if err != nil {
		return err
	}

	cmd := exec.Command(s.brewPath, BuildBundleInstallArgs(filePath, cleanup)...)
	system.ApplyEnvironment(cmd, s.getBrewEnvFunc())
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("brew bundle install failed: %v\nOutput: %s", err, string(output))
	}

	return nil
}

// DeleteSnapshot removes a saved snapshot file.
func (s *SnapshotService) DeleteSnapshot(fileName string) error {
	filePath, err := s.resolveSnapshotPath(fileName)
	if err != nil {
		return err
	}
	if err := os.Remove(filePath); err != nil {
		return fmt.Errorf("failed to delete snapshot: %w", err)
	}
	return nil
}

// RevealSnapshot opens the platform file manager with the given snapshot
// file selected (Finder on macOS, Explorer on Windows), or its containing
// folder on Linux, where desktop environments don't share a single
// "select this file" convention.
func (s *SnapshotService) RevealSnapshot(fileName string) error {
	filePath, err := s.resolveSnapshotPath(fileName)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filePath); err != nil {
		return fmt.Errorf("snapshot file not found: %w", err)
	}

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", "-R", filePath)
	case "windows":
		cmd = exec.Command("explorer", "/select,"+filePath)
	case "linux":
		cmd = exec.Command("xdg-open", filepath.Dir(filePath))
	default:
		return fmt.Errorf("unsupported operating system: %s", runtime.GOOS)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to reveal snapshot: %w", err)
	}
	return nil
}

// resolveSnapshotPath joins fileName against the snapshots directory,
// rejecting anything that doesn't match the known snapshot filename shape to
// prevent path traversal (e.g. "../../etc/passwd").
func (s *SnapshotService) resolveSnapshotPath(fileName string) (string, error) {
	if !snapshotFileNamePattern.MatchString(fileName) {
		return "", fmt.Errorf("invalid snapshot file name: %s", fileName)
	}
	dir, err := s.getSnapshotsDir()
	if err != nil {
		return "", fmt.Errorf("failed to resolve snapshots directory: %w", err)
	}
	return filepath.Join(dir, fileName), nil
}
