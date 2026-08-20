package claude

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/tracecapture"
)

type sourceFile struct {
	path      string
	sessionID string
	agentID   string
	size      int64
	identity  fileIdentity
	explicit  bool
}

var errSourceTreeUnavailable = errors.New("Claude Code source tree unavailable")

// ResolveConfigRoot resolves an explicit root first, then Claude Code's
// documented override, then the current user's default data directory.
func ResolveConfigRoot(explicit string) (string, error) {
	root := strings.TrimSpace(explicit)
	if root == "" {
		root = strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	}
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errors.New("resolve Claude Code config root")
		}
		root = filepath.Join(home, ".claude")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", errors.New("resolve Claude Code config root")
	}
	return filepath.Clean(absolute), nil
}

func (c Collector) Discover(ctx context.Context, selection Selection) (Plan, error) {
	if err := validateSelection(selection); err != nil {
		return Plan{}, err
	}
	root, err := ResolveConfigRoot(c.ConfigRoot)
	if err != nil {
		return Plan{}, fmt.Errorf("%w: config root is invalid", ErrInvalidSelection)
	}
	if err := validateSourceDirectory(root); err != nil {
		if os.IsNotExist(err) && len(selection.ExplicitSessions) == 0 {
			return Plan{selection: cloneSelection(selection)}, nil
		}
		return Plan{}, errors.New("Claude Code config root is unavailable or unsafe")
	}
	projects := filepath.Join(filepath.Clean(root), "projects")
	if err := validateSourceDirectory(projects); err != nil {
		if os.IsNotExist(err) && len(selection.ExplicitSessions) == 0 {
			return Plan{selection: cloneSelection(selection)}, nil
		}
		return Plan{}, err
	}

	explicit := make(map[string]struct{}, len(selection.ExplicitSessions))
	for _, sessionID := range selection.ExplicitSessions {
		explicit[sessionID] = struct{}{}
	}
	responseIDs := traceResponseIDs(selection.Traces)
	hasResponseFallback := len(responseIDs) > 0

	plan := Plan{selection: cloneSelection(selection)}
	foundExplicit := make(map[string]bool, len(explicit))
	err = filepath.WalkDir(projects, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errSourceTreeUnavailable
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == projects {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			rel, relErr := filepath.Rel(projects, path)
			if relErr != nil {
				return relErr
			}
			if len(splitPath(rel)) > 3 {
				return filepath.SkipDir
			}
			return nil
		}
		sessionID, agentID, recognized := recognizedSessionPath(projects, path)
		if !recognized {
			return nil
		}
		info, identity, inspectErr := inspectSourceFile(path)
		if inspectErr != nil {
			return nil
		}
		_, isExplicit := explicit[sessionID]
		if isExplicit {
			foundExplicit[sessionID] = true
		}
		keyedScope := c.matchesKeyedScope(selection.Traces, sessionID, agentID)
		mtimeScope := hasResponseFallback && !info.ModTime().Before(selection.Since)
		if !isExplicit && !keyedScope && !mtimeScope {
			return nil
		}
		if len(plan.files) >= MaxSourceFiles {
			return fmt.Errorf("%w: more than %d native source files", ErrLimitExceeded, MaxSourceFiles)
		}
		if info.Size() < 0 || plan.CandidateBytes > MaxSourceBytes-info.Size() {
			return fmt.Errorf("%w: native source bytes exceed %d", ErrLimitExceeded, MaxSourceBytes)
		}
		plan.files = append(plan.files, sourceFile{
			path:      path,
			sessionID: sessionID,
			agentID:   agentID,
			size:      info.Size(),
			identity:  identity,
			explicit:  isExplicit,
		})
		plan.CandidateBytes += info.Size()
		return nil
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(err, ErrLimitExceeded) {
			return Plan{}, err
		}
		return Plan{}, errSourceTreeUnavailable
	}
	for sessionID := range explicit {
		if !foundExplicit[sessionID] {
			return Plan{}, fmt.Errorf("%w: requested Claude Code session was not found", ErrExplicitSource)
		}
	}
	slices.SortFunc(plan.files, func(a, b sourceFile) int {
		return strings.Compare(a.path, b.path)
	})
	plan.CandidateFileCount = len(plan.files)
	return plan, nil
}

func validateSelection(selection Selection) error {
	if selection.Since.IsZero() || selection.Until.IsZero() || !selection.Since.Before(selection.Until) {
		return fmt.Errorf("%w: since and until must define a nonempty interval", ErrInvalidSelection)
	}
	if selection.Until.Sub(selection.Since) > 30*24*time.Hour {
		return fmt.Errorf("%w: interval exceeds 30 days", ErrInvalidSelection)
	}
	if len(selection.Traces) > MaxNativeTurns || len(selection.ExplicitSessions) > MaxSourceFiles {
		return fmt.Errorf("%w: native selection count", ErrLimitExceeded)
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
		if trace.StartedAt.IsZero() || trace.CompletedAt.IsZero() || trace.CompletedAt.Before(trace.StartedAt) {
			return fmt.Errorf("%w: trace interval is invalid", ErrInvalidSelection)
		}
		if len(trace.ResponseBody) > 16<<20 {
			return fmt.Errorf("%w: response body exceeds trace limit", ErrInvalidSelection)
		}
		if err := validateNativeCorrelation(trace.NativeCorrelation); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidSelection, err)
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
		Traces:           make([]TraceReference, len(selection.Traces)),
	}
	for i, trace := range selection.Traces {
		cloned.Traces[i] = trace
		cloned.Traces[i].ResponseBody = slices.Clone(trace.ResponseBody)
		if trace.NativeCorrelation != nil {
			correlation := *trace.NativeCorrelation
			correlation.Session = cloneStringPointer(trace.NativeCorrelation.Session)
			correlation.Turn = cloneStringPointer(trace.NativeCorrelation.Turn)
			correlation.Agent = cloneStringPointer(trace.NativeCorrelation.Agent)
			cloned.Traces[i].NativeCorrelation = &correlation
		}
	}
	return cloned
}

func validateNativeCorrelation(correlation *tracecapture.NativeCorrelationV1) error {
	if correlation == nil {
		return nil
	}
	if correlation.Scheme != "hmac-sha256-v1" || len(correlation.KeyID) != 16 {
		return errors.New("native correlation scheme or key ID is invalid")
	}
	if _, err := hex.DecodeString(correlation.KeyID); err != nil || correlation.KeyID != strings.ToLower(correlation.KeyID) {
		return errors.New("native correlation key ID is invalid")
	}
	present := false
	for _, value := range []*string{correlation.Session, correlation.Turn, correlation.Agent} {
		if value == nil {
			continue
		}
		present = true
		if !validEventID(*value) {
			return errors.New("native correlation hash is invalid")
		}
	}
	if !present {
		return errors.New("native correlation has no join values")
	}
	return nil
}

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func (c Collector) matchesKeyedScope(traces []TraceReference, sessionID, agentID string) bool {
	if c.CorrelationKey == nil {
		return false
	}
	sessionHash, _ := c.CorrelationKey.Hash(ClientName, "session", sessionID)
	agentHash := ""
	if agentID != "" {
		agentHash, _ = c.CorrelationKey.Hash(ClientName, "agent", agentID)
	}
	for _, trace := range traces {
		correlation := trace.NativeCorrelation
		if correlation == nil || correlation.KeyID != c.CorrelationKey.ID() {
			continue
		}
		if correlation.Session != nil && *correlation.Session == sessionHash {
			if correlation.Agent == nil || agentHash == *correlation.Agent {
				return true
			}
		}
		if correlation.Session == nil && correlation.Agent != nil && agentHash == *correlation.Agent {
			return true
		}
	}
	return false
}

func recognizedSessionPath(projects, path string) (string, string, bool) {
	rel, err := filepath.Rel(projects, path)
	if err != nil || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", false
	}
	parts := splitPath(rel)
	switch {
	case len(parts) == 2 && strings.HasSuffix(parts[1], ".jsonl"):
		sessionID := strings.TrimSuffix(parts[1], ".jsonl")
		return sessionID, "", validUUID(sessionID)
	case len(parts) == 3 && parts[2] == "main.jsonl":
		return parts[1], "", validUUID(parts[1])
	case len(parts) == 4 && parts[2] == "subagents" &&
		strings.HasPrefix(parts[3], "agent-") && strings.HasSuffix(parts[3], ".jsonl"):
		agentID := strings.TrimSuffix(strings.TrimPrefix(parts[3], "agent-"), ".jsonl")
		return parts[1], agentID, validUUID(parts[1]) && validAgentID(agentID)
	default:
		return "", "", false
	}
}

func splitPath(path string) []string {
	if path == "." || path == "" {
		return nil
	}
	return strings.Split(filepath.Clean(path), string(filepath.Separator))
}

func validUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	_, err := hex.DecodeString(strings.ReplaceAll(value, "-", ""))
	return err == nil && value == strings.ToLower(value)
}

func validAgentID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_') {
			return false
		}
	}
	return true
}

func validEventID(value string) bool {
	if len(value) != 32 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
