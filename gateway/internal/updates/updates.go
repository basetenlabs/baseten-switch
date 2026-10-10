// Package updates discovers published releases without installation or runtime changes.
package updates

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	CheckInterval  = 24 * time.Hour
	FailureBackoff = time.Hour
	maxResponse    = 128 << 10
	apiBase        = "https://api.github.com"
	releaseBase    = "https://github.com/basetenlabs/baseten-switch/releases/tag/"
)

var numericVersion = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// Result retains a confirmed target even if this CLI is already current, so an
// older running app can compare its own bundle version independently.
type Result struct {
	CurrentVersion   string     `json:"current_version"`
	AvailableVersion string     `json:"available_version,omitempty"`
	ReleaseURL       string     `json:"release_url,omitempty"`
	CheckedAt        *time.Time `json:"checked_at,omitempty"`
	LastAttemptAt    *time.Time `json:"last_attempt_at,omitempty"`
	NextCheckAt      *time.Time `json:"next_check_at,omitempty"`
	AutomaticCheck   bool       `json:"automatic_check"`
	InstallSource    string     `json:"install_source"`
	Status           string     `json:"status"`
	Error            string     `json:"error,omitempty"`
}

type cache struct {
	Version        string     `json:"version,omitempty"`
	CheckedAt      *time.Time `json:"checked_at,omitempty"`
	AttemptAt      *time.Time `json:"attempt_at,omitempty"`
	NextAt         *time.Time `json:"next_at,omitempty"`
	Error          string     `json:"error,omitempty"`
	NoticedVersion string     `json:"noticed_version,omitempty"`
	NoticedAt      *time.Time `json:"noticed_at,omitempty"`
}

type Checker struct {
	Root           string
	CurrentVersion string
	InstallSource  string
	Private        bool
	Client         *http.Client
	Now            func() time.Time
	// APIBase is an injection seam for hermetic tests, never read from env.
	APIBase string
}

func ParseVersion(s string) ([3]uint64, bool) {
	var result [3]uint64
	match := numericVersion.FindStringSubmatch(s)
	if match == nil {
		return result, false
	}
	for i := range result {
		n, err := strconv.ParseUint(match[i+1], 10, 64)
		if err != nil {
			return result, false
		}
		result[i] = n
	}
	return result, true
}

func IsNewer(target, current string) bool {
	a, ok := ParseVersion(target)
	if !ok {
		return false
	}
	b, ok := ParseVersion(current)
	if !ok {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

func (c Checker) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}
func (c Checker) eligible() bool { _, ok := ParseVersion(c.CurrentVersion); return ok && !c.Private }

func (c Checker) automatic() (bool, error) {
	var preference struct {
		Automatic bool `json:"automatic_check"`
	}
	b, err := os.ReadFile(filepath.Join(c.Root, "preference.json"))
	if errors.Is(err, os.ErrNotExist) {
		return c.eligible(), nil
	}
	if err != nil {
		return false, fmt.Errorf("cannot read update preference: %w", err)
	}
	if err := json.Unmarshal(b, &preference); err != nil {
		return false, errors.New("invalid update preference")
	}
	return preference.Automatic, nil
}

func (c Checker) SetAutomatic(enabled bool) error {
	return writeJSON(filepath.Join(c.Root, "preference.json"), struct {
		Automatic bool `json:"automatic_check"`
	}{enabled})
}

// Snapshot projects metadata and preference without making a network request.
func (c Checker) Snapshot() (Result, error) {
	automatic, err := c.automatic()
	r := c.result(c.readCache(), automatic)
	if err != nil {
		r.Error = err.Error()
	}
	return r, err
}

func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".update-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func (c Checker) lock(ctx context.Context) (*os.File, error) {
	if err := os.MkdirAll(c.Root, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(c.Root, "check.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func unlock(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }

func (c Checker) readCache() cache {
	var state cache
	b, err := os.ReadFile(filepath.Join(c.Root, "cache.json"))
	if err != nil || json.Unmarshal(b, &state) != nil {
		return cache{}
	}
	if state.Version != "" {
		if _, ok := ParseVersion(state.Version); !ok || !strings.HasPrefix(state.Version, "v") || state.CheckedAt == nil {
			return cache{}
		}
	}
	return state
}

func (c Checker) result(state cache, automatic bool) Result {
	source := c.InstallSource
	if source != "homebrew" && source != "nix" {
		source = "unknown"
	}
	r := Result{CurrentVersion: c.CurrentVersion, AvailableVersion: state.Version, CheckedAt: state.CheckedAt, LastAttemptAt: state.AttemptAt, NextCheckAt: state.NextAt, AutomaticCheck: automatic, InstallSource: source, Error: state.Error, Status: "unknown"}
	if state.Version != "" {
		r.ReleaseURL = releaseBase + state.Version
		r.Status = "current"
		if IsNewer(state.Version, c.CurrentVersion) {
			r.Status = "available"
		}
	}
	if !c.eligible() {
		r.Status = "unsupported"
	} else if !automatic {
		r.Status = "disabled"
	}
	return r
}

// Check serializes discovery across app and CLI processes. Failure does not
// overwrite the confirmed target or its successful check time.
func (c Checker) Check(ctx context.Context, refresh bool) (Result, error) {
	automatic, err := c.automatic()
	if err != nil {
		r := c.result(c.readCache(), false)
		r.Error = err.Error()
		return r, err
	}
	state := c.readCache()
	if c.Private || (!refresh && (!automatic || !c.eligible())) {
		return c.result(state, automatic), nil
	}
	if !refresh && state.NextAt != nil && c.now().Before(*state.NextAt) {
		r := c.result(state, automatic)
		if r.Error != "" {
			return r, errors.New(r.Error)
		}
		return r, nil
	}
	f, err := c.lock(ctx)
	if err != nil {
		r := c.result(state, automatic)
		r.Error = "update check is busy or timed out"
		return r, err
	}
	defer unlock(f)
	state = c.readCache()
	// A concurrent preference change can stop a pending automatic request.
	automatic, err = c.automatic()
	if err != nil {
		return c.result(state, false), err
	}
	if !refresh && (!automatic || (state.NextAt != nil && c.now().Before(*state.NextAt))) {
		r := c.result(state, automatic)
		if r.Error != "" {
			return r, errors.New(r.Error)
		}
		return r, nil
	}
	now := c.now()
	state.AttemptAt = &now
	tag, retryAt, fetchErr := c.fetch(ctx)
	if fetchErr != nil {
		state.Error = fetchErr.Error()
		next := now.Add(FailureBackoff)
		if retryAt.After(next) {
			next = retryAt
		}
		state.NextAt = &next
	} else {
		state.Version = tag
		state.CheckedAt = &now
		state.Error = ""
		next := now.Add(CheckInterval)
		state.NextAt = &next
	}
	if err := writeJSON(filepath.Join(c.Root, "cache.json"), state); err != nil {
		r := c.result(state, automatic)
		r.Error = "cannot save update check"
		return r, err
	}
	// Preference writes are independent of the network lock. Project the latest
	// preference so a slow check cannot revert a concurrently changed UI toggle.
	if latest, err := c.automatic(); err == nil {
		automatic = latest
	} else {
		r := c.result(state, false)
		r.Error = err.Error()
		return r, err
	}
	return c.result(state, automatic), fetchErr
}

// TakeNotice deduplicates human terminal hints by target and day.
func (c Checker) TakeNotice(ctx context.Context, target string) bool {
	f, err := c.lock(ctx)
	if err != nil {
		return false
	}
	defer unlock(f)
	if automatic, err := c.automatic(); err != nil || !automatic || !c.eligible() {
		return false
	}
	state := c.readCache()
	now := c.now()
	if state.Version != target || !IsNewer(target, c.CurrentVersion) {
		return false
	}
	if state.NoticedVersion == target && state.NoticedAt != nil && now.Sub(*state.NoticedAt) < CheckInterval {
		return false
	}
	state.NoticedVersion = target
	state.NoticedAt = &now
	return writeJSON(filepath.Join(c.Root, "cache.json"), state) == nil
}

func (c Checker) fetch(ctx context.Context) (string, time.Time, error) {
	var formula struct {
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	retry, err := c.get(ctx, "/repos/basetenlabs/homebrew-baseten/contents/Formula/baseten-switch.rb?ref=main", &formula)
	if err != nil {
		return "", retry, err
	}
	if formula.Encoding != "base64" {
		return "", time.Time{}, errors.New("invalid Homebrew formula encoding")
	}
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(formula.Content, "\n", ""))
	if err != nil {
		return "", time.Time{}, errors.New("invalid Homebrew formula content")
	}
	tag, err := parseFormula(string(data))
	if err != nil {
		return "", time.Time{}, err
	}
	var release struct {
		Tag         string     `json:"tag_name"`
		Draft       bool       `json:"draft"`
		URL         string     `json:"html_url"`
		PublishedAt *time.Time `json:"published_at"`
	}
	retry, err = c.get(ctx, "/repos/basetenlabs/baseten-switch/releases/tags/"+tag, &release)
	if err != nil {
		return "", retry, err
	}
	if release.Tag != tag || release.Draft || release.PublishedAt == nil || release.URL != releaseBase+tag {
		return "", time.Time{}, errors.New("Homebrew version has no matching published release")
	}
	return tag, time.Time{}, nil
}

func parseFormula(text string) (string, error) {
	// Match the literal canonical URL, never evaluate downloaded Ruby.
	if len(regexp.MustCompile(`(?m)^\s*url\b`).FindAllString(text, -1)) != 1 {
		return "", errors.New("ambiguous Homebrew release URL")
	}
	urlLine := regexp.MustCompile(`(?m)^\s*url\s+"(https://github\.com/basetenlabs/baseten-switch/releases/download/(v[0-9]+\.[0-9]+\.[0-9]+)/baseten-switch_([0-9]+\.[0-9]+\.[0-9]+)_darwin_universal\.zip)"\s*$`)
	matches := urlLine.FindAllStringSubmatch(text, -1)
	if len(matches) != 1 || strings.TrimPrefix(matches[0][2], "v") != matches[0][3] {
		return "", errors.New("invalid canonical Homebrew release URL")
	}
	tag := matches[0][2]
	if _, ok := ParseVersion(tag); !ok {
		return "", errors.New("invalid Homebrew release version")
	}
	return tag, nil
}

func (c Checker) get(ctx context.Context, path string, into any) (time.Time, error) {
	base := apiBase
	if c.APIBase != "" {
		base = c.APIBase
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return time.Time{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "baseten-switch-update-check")
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := client.Do(req)
	if err != nil {
		return time.Time{}, errors.New("unable to reach release metadata")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		now := c.now()
		retry := time.Time{}
		if seconds, err := strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 64); err == nil && seconds > 0 && seconds < 365*24*3600 {
			retry = now.Add(time.Duration(seconds) * time.Second)
		} else if parsed, err := http.ParseTime(resp.Header.Get("Retry-After")); err == nil {
			retry = parsed
		}
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil && (resp.StatusCode == 429 || resp.StatusCode == 403) && time.Unix(reset, 0).After(retry) {
			retry = time.Unix(reset, 0)
		}
		return retry, fmt.Errorf("release metadata returned HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return time.Time{}, errors.New("unable to read release metadata")
	}
	if len(b) > maxResponse {
		return time.Time{}, errors.New("release metadata exceeds size limit")
	}
	if err := json.Unmarshal(b, into); err != nil {
		return time.Time{}, errors.New("invalid release metadata")
	}
	return time.Time{}, nil
}
