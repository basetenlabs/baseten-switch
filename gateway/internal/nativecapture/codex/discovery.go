package codex

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const maxDiscoveryEntries = 100_000

type sourceFile struct {
	path       string
	sessionID  string
	size       int64
	modTime    time.Time
	identity   fileIdentity
	explicit   bool
	archived   bool
	partition  time.Time
	keyedScope bool
}

// ResolveCodexHome applies Codex's supported environment override before the
// documented per-user default. It does not inspect configuration files.
func ResolveCodexHome(explicit string) (string, error) {
	home := strings.TrimSpace(explicit)
	if home == "" {
		home = strings.TrimSpace(os.Getenv("CODEX_HOME"))
	}
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve Codex home: %w", err)
		}
		home = filepath.Join(userHome, ".codex")
	}
	absolute, err := filepath.Abs(home)
	if err != nil {
		return "", fmt.Errorf("resolve Codex home: %w", err)
	}
	return filepath.Clean(absolute), nil
}

func (c Collector) Discover(ctx context.Context, selection Selection) (Plan, error) {
	if err := validateSelection(selection); err != nil {
		return Plan{}, err
	}
	root, err := ResolveCodexHome(c.CodexHome)
	if err != nil {
		return Plan{}, err
	}
	if err := validateSourceDirectory(root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if len(selection.ExplicitSessions) == 0 {
				return Plan{selection: cloneSelection(selection)}, nil
			}
			return Plan{}, fmt.Errorf("%w: Codex home is unavailable", ErrExplicitSource)
		}
		return Plan{}, errors.New("Codex home is unavailable or unsafe")
	}

	explicit := make(map[string]struct{}, len(selection.ExplicitSessions))
	for _, sessionID := range selection.ExplicitSessions {
		explicit[sessionID] = struct{}{}
	}
	foundExplicit := make(map[string]bool, len(explicit))
	plan := Plan{selection: cloneSelection(selection)}
	entries := 0

	activeRoot := filepath.Join(root, "sessions")
	if statErr := c.walkActive(ctx, activeRoot, selection, explicit, foundExplicit, &plan, &entries); statErr != nil {
		return Plan{}, statErr
	}
	if selection.IncludeArchived {
		archiveRoot := filepath.Join(root, "archived_sessions")
		if statErr := c.walkArchived(ctx, archiveRoot, selection, explicit, foundExplicit, &plan, &entries); statErr != nil {
			return Plan{}, statErr
		}
	}
	for sessionID := range explicit {
		if !foundExplicit[sessionID] {
			return Plan{}, fmt.Errorf("%w: requested Codex session was not found", ErrExplicitSource)
		}
	}
	slices.SortFunc(plan.files, func(a, b sourceFile) int {
		return strings.Compare(a.path, b.path)
	})
	plan.CandidateFileCount = len(plan.files)
	return plan, nil
}

func (c Collector) walkActive(
	ctx context.Context,
	root string,
	selection Selection,
	explicit map[string]struct{},
	foundExplicit map[string]bool,
	plan *Plan,
	entries *int,
) error {
	if err := validateSourceDirectory(root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return errors.New("Codex sessions directory is unavailable or unsafe")
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errors.New("cannot inspect a Codex session entry")
		}
		if err := ctx.Err(); err != nil {
			return errors.New("cannot resolve a Codex session entry")
		}
		*entries = *entries + 1
		if *entries > maxDiscoveryEntries {
			return fmt.Errorf("%w: too many Codex session entries", ErrLimitExceeded)
		}
		if path == root {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		parts := splitPath(rel)
		if entry.IsDir() {
			if len(parts) > 3 || !validPartitionPrefix(parts) {
				return filepath.SkipDir
			}
			return nil
		}
		if len(parts) != 4 {
			return nil
		}
		partition, ok := parsePartition(parts[:3])
		if !ok {
			return nil
		}
		sessionID, ok := rolloutSessionID(parts[3])
		if !ok {
			return nil
		}
		filenamePartition, ok := rolloutPartition(parts[3])
		if !ok || !filenamePartition.Equal(partition) {
			return nil
		}
		return c.considerSource(path, sessionID, partition, false, selection, explicit, foundExplicit, plan)
	})
}

func (c Collector) walkArchived(
	ctx context.Context,
	root string,
	selection Selection,
	explicit map[string]struct{},
	foundExplicit map[string]bool,
	plan *Plan,
	entries *int,
) error {
	if err := validateSourceDirectory(root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return errors.New("Codex archived sessions directory is unavailable or unsafe")
	}
	entriesList, err := os.ReadDir(root)
	if err != nil {
		return errors.New("cannot inspect Codex archived sessions")
	}
	for _, entry := range entriesList {
		if err := ctx.Err(); err != nil {
			return err
		}
		*entries = *entries + 1
		if *entries > maxDiscoveryEntries {
			return fmt.Errorf("%w: too many Codex session entries", ErrLimitExceeded)
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		sessionID, ok := rolloutSessionID(entry.Name())
		if !ok {
			continue
		}
		partition, ok := rolloutPartition(entry.Name())
		if !ok {
			continue
		}
		if err := c.considerSource(filepath.Join(root, entry.Name()), sessionID, partition, true, selection, explicit, foundExplicit, plan); err != nil {
			return err
		}
	}
	return nil
}

func (c Collector) considerSource(
	path, sessionID string,
	partition time.Time,
	archived bool,
	selection Selection,
	explicit map[string]struct{},
	foundExplicit map[string]bool,
	plan *Plan,
) error {
	info, identity, err := inspectSourceFile(path)
	if err != nil {
		return nil
	}
	_, isExplicit := explicit[sessionID]
	if isExplicit {
		foundExplicit[sessionID] = true
	}
	keyed := c.matchesKeyedSession(selection.Traces, sessionID)
	timeScoped := partitionIntersects(partition, selection.Since, selection.Until) ||
		(!info.ModTime().Before(selection.Since) && info.ModTime().Before(selection.Until.Add(24*time.Hour)))
	if !isExplicit && !keyed && !timeScoped {
		return nil
	}
	if len(plan.files) >= MaxSourceFiles {
		return fmt.Errorf("%w: more than %d Codex source files", ErrLimitExceeded, MaxSourceFiles)
	}
	if info.Size() < 0 || plan.CandidateBytes > MaxSourceBytes-info.Size() {
		return fmt.Errorf("%w: Codex source bytes exceed %d", ErrLimitExceeded, MaxSourceBytes)
	}
	plan.files = append(plan.files, sourceFile{
		path:       path,
		sessionID:  sessionID,
		size:       info.Size(),
		modTime:    info.ModTime().UTC(),
		identity:   identity,
		explicit:   isExplicit,
		archived:   archived,
		partition:  partition,
		keyedScope: keyed,
	})
	plan.CandidateBytes += info.Size()
	return nil
}

func validateSelection(selection Selection) error {
	if selection.Since.IsZero() || selection.Until.IsZero() || !selection.Since.Before(selection.Until) {
		return fmt.Errorf("%w: since and until must define a nonempty interval", ErrInvalidSelection)
	}
	if selection.Until.Sub(selection.Since) > 30*24*time.Hour {
		return fmt.Errorf("%w: interval exceeds 30 days", ErrInvalidSelection)
	}
	seenEvents := make(map[string]struct{}, len(selection.Traces))
	for _, trace := range selection.Traces {
		if !validEventID(trace.EventID) {
			return fmt.Errorf("%w: invalid Switch event ID", ErrInvalidSelection)
		}
		if _, exists := seenEvents[trace.EventID]; exists {
			return fmt.Errorf("%w: duplicate Switch event ID", ErrInvalidSelection)
		}
		seenEvents[trace.EventID] = struct{}{}
		if trace.StartedAt.IsZero() || trace.CompletedAt.Before(trace.StartedAt) {
			return fmt.Errorf("%w: invalid trace interval", ErrInvalidSelection)
		}
		if trace.StartedAt.Before(selection.Since) || !trace.StartedAt.Before(selection.Until) {
			return fmt.Errorf("%w: trace admission is outside the selection interval", ErrInvalidSelection)
		}
		if len(trace.RequestBody) > 16<<20 {
			return fmt.Errorf("%w: request body exceeds trace limit", ErrInvalidSelection)
		}
	}
	seenSessions := make(map[string]struct{}, len(selection.ExplicitSessions))
	for _, sessionID := range selection.ExplicitSessions {
		if !validUUID(sessionID) {
			return fmt.Errorf("%w: explicit session ID is not a UUID", ErrInvalidSelection)
		}
		if _, exists := seenSessions[sessionID]; exists {
			return fmt.Errorf("%w: duplicate explicit session ID", ErrInvalidSelection)
		}
		seenSessions[sessionID] = struct{}{}
	}
	if len(selection.Traces) == 0 && len(selection.ExplicitSessions) == 0 {
		return fmt.Errorf("%w: no trace or explicit session selection", ErrInvalidSelection)
	}
	return nil
}

func cloneSelection(selection Selection) Selection {
	cloned := Selection{
		Since:            selection.Since.UTC(),
		Until:            selection.Until.UTC(),
		ExplicitSessions: slices.Clone(selection.ExplicitSessions),
		IncludeArchived:  selection.IncludeArchived,
		Traces:           make([]TraceReference, len(selection.Traces)),
	}
	for i, trace := range selection.Traces {
		cloned.Traces[i] = trace
		cloned.Traces[i].RequestBody = slices.Clone(trace.RequestBody)
		if trace.NativeCorrelation != nil {
			correlation := *trace.NativeCorrelation
			cloned.Traces[i].NativeCorrelation = &correlation
		}
	}
	return cloned
}

func (c Collector) matchesKeyedSession(traces []TraceReference, sessionID string) bool {
	if c.CorrelationKey == nil {
		return false
	}
	hash, err := c.CorrelationKey.Hash(ClientName, "session", sessionID)
	if err != nil {
		return false
	}
	for _, trace := range traces {
		correlation := trace.NativeCorrelation
		if correlation != nil && correlation.KeyID == c.CorrelationKey.ID() &&
			correlation.Session != nil && *correlation.Session == hash {
			return true
		}
	}
	return false
}

func validateSourceDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("Codex source root must be a non-symlink directory")
	}
	_, err = identityFromInfo(info)
	return err
}

func inspectSourceFile(path string) (os.FileInfo, fileIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fileIdentity{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fileIdentity{}, errors.New("Codex source must be a regular non-symlink file")
	}
	identity, err := identityFromInfo(info)
	return info, identity, err
}

func validPartitionPrefix(parts []string) bool {
	for i, part := range parts {
		if (i == 0 && len(part) != 4) || (i > 0 && len(part) != 2) {
			return false
		}
		if _, err := strconv.Atoi(part); err != nil {
			return false
		}
	}
	return true
}

func parsePartition(parts []string) (time.Time, bool) {
	if len(parts) != 3 || !validPartitionPrefix(parts) {
		return time.Time{}, false
	}
	year, _ := strconv.Atoi(parts[0])
	month, _ := strconv.Atoi(parts[1])
	day, _ := strconv.Atoi(parts[2])
	value := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	return value, value.Year() == year && int(value.Month()) == month && value.Day() == day
}

func partitionIntersects(partition, since, until time.Time) bool {
	if partition.IsZero() {
		return false
	}
	dayEnd := partition.Add(24 * time.Hour)
	return partition.Before(until.UTC()) && dayEnd.After(since.UTC())
}

func rolloutSessionID(name string) (string, bool) {
	if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") || len(name) > 180 {
		return "", false
	}
	for _, char := range name {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.') {
			return "", false
		}
	}
	for index := 0; index+36 <= len(name); index++ {
		candidate := name[index : index+36]
		if validUUID(candidate) {
			return candidate, true
		}
	}
	return "", false
}

func rolloutPartition(name string) (time.Time, bool) {
	const prefix = "rollout-"
	if !strings.HasPrefix(name, prefix) || len(name) < len(prefix)+10 {
		return time.Time{}, false
	}
	parsed, err := time.Parse("2006-01-02", name[len(prefix):len(prefix)+10])
	return parsed.UTC(), err == nil
}

func splitPath(path string) []string {
	if path == "." || path == "" {
		return nil
	}
	return strings.Split(filepath.Clean(path), string(filepath.Separator))
}

func validUUID(value string) bool {
	if len(value) != 36 || value != strings.ToLower(value) || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	_, err := hex.DecodeString(strings.ReplaceAll(value, "-", ""))
	return err == nil
}

func validEventID(value string) bool {
	if len(value) != 32 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
