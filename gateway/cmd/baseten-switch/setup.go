package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/auth"
	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
)

const minimumBasetenCLIVersion = "0.3.0"

var errSetupCredentialUnavailable = errors.New("current Baseten credential is missing or unusable")

type setupDependencies struct {
	findBaseten    func() (string, error)
	basetenVersion func(string) string
	loadCredential func() (string, error)
	login          func(string) error
	configPath     func() string
	stat           func(string) (os.FileInfo, error)
	initConfig     func(string, bool, io.Writer) int
}

func defaultSetupDependencies() setupDependencies {
	return setupDependencies{
		findBaseten:    lookBaseten,
		basetenVersion: basetenCLIVersion,
		loadCredential: loadCurrentSetupCredential,
		login:          runBasetenLogin,
		configPath: func() string {
			return envDefault("BASETEN_SWITCH_CONFIG_PATH", config.DefaultPath())
		},
		stat:       os.Stat,
		initConfig: runConfigInit,
	}
}

func cmdSetup(args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "usage: baseten-switch setup")
		return 2
	}
	return runSetup(defaultSetupDependencies(), os.Stdout, os.Stderr)
}

func runSetup(deps setupDependencies, out, errOut io.Writer) int {
	bin, err := deps.findBaseten()
	if err != nil {
		fmt.Fprintf(errOut, "setup: no 'baseten' CLI on PATH. Install it with '%s'.\n", basetenBrewHint)
		return 1
	}

	versionOutput := strings.TrimSpace(deps.basetenVersion(bin))
	version, err := parseSemanticVersion(versionOutput)
	if err != nil {
		fmt.Fprintf(errOut, "setup: could not determine a semantic version from '%s --version' output %q; baseten CLI v%s or newer is required.\n", bin, versionOutput, minimumBasetenCLIVersion)
		return 1
	}
	minimum, _ := parseSemanticVersion(minimumBasetenCLIVersion)
	if version.less(minimum) {
		fmt.Fprintf(errOut, "setup: baseten CLI v%s is too old; v%s or newer is required. Upgrade it with 'brew upgrade basetenlabs/baseten/baseten'.\n", version, minimum)
		return 1
	}
	fmt.Fprintf(out, "Baseten CLI: %s (v%s)\n", bin, version)

	credential, err := deps.loadCredential()
	if err != nil {
		fmt.Fprintf(out, "Baseten credential: unavailable (%v); starting 'baseten auth login'.\n", err)
		if err := deps.login(bin); err != nil {
			fmt.Fprintf(errOut, "setup: baseten auth login failed: %v\n", err)
			return 1
		}
		credential, err = deps.loadCredential()
		if err != nil {
			fmt.Fprintf(errOut, "setup: baseten auth login completed, but the current credential is still unavailable: %v\n", err)
			return 1
		}
	}
	fmt.Fprintf(out, "Baseten credential: %s\n", credential)

	path := deps.configPath()
	if _, err := deps.stat(path); err == nil {
		fmt.Fprintf(out, "Gateway config: using existing %s\n", path)
	} else if !os.IsNotExist(err) {
		fmt.Fprintf(errOut, "setup: inspect gateway config %s: %v\n", path, err)
		return 1
	} else {
		if code := deps.initConfig(path, false, io.Discard); code != 0 {
			fmt.Fprintf(errOut, "setup: could not initialize gateway config at %s\n", path)
			return 1
		}
		fmt.Fprintf(out, "Gateway config: created %s\n", path)
	}

	fmt.Fprintln(out, "\nNext commands:")
	fmt.Fprintln(out, "baseten-switch up --install")
	fmt.Fprintln(out, "baseten-switch claude on")
	fmt.Fprintln(out, "baseten-switch doctor --probe")
	return 0
}

func loadCurrentSetupCredential() (string, error) {
	token, _, err := auth.Load("")
	if err != nil {
		var apiKeyProfile *auth.APIKeyProfileError
		if errors.As(err, &apiKeyProfile) && strings.TrimSpace(apiKeyProfile.Key) != "" {
			return fmt.Sprintf("current API key profile %q is available", apiKeyProfile.Profile), nil
		}
		return "", fmt.Errorf("%w: %v", errSetupCredentialUnavailable, err)
	}
	if token == nil || strings.TrimSpace(token.AccessToken) == "" {
		return "", errSetupCredentialUnavailable
	}
	if !token.Expiry.IsZero() && time.Now().After(token.Expiry) && strings.TrimSpace(token.RefreshToken) == "" {
		return "", fmt.Errorf("%w: OAuth access token is expired and has no refresh token", errSetupCredentialUnavailable)
	}
	return "current OAuth credential is available", nil
}

type semanticVersion struct {
	major      uint64
	minor      uint64
	patch      uint64
	prerelease string
}

func (v semanticVersion) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch)
	if v.prerelease != "" {
		s += "-" + v.prerelease
	}
	return s
}

func (v semanticVersion) less(other semanticVersion) bool {
	if v.major != other.major {
		return v.major < other.major
	}
	if v.minor != other.minor {
		return v.minor < other.minor
	}
	if v.patch != other.patch {
		return v.patch < other.patch
	}
	if v.prerelease == "" {
		return false
	}
	if other.prerelease == "" {
		return true
	}
	left := strings.Split(v.prerelease, ".")
	right := strings.Split(other.prerelease, ".")
	for i := 0; i < len(left) && i < len(right); i++ {
		if left[i] == right[i] {
			continue
		}
		leftNumber, leftErr := strconv.ParseUint(left[i], 10, 64)
		rightNumber, rightErr := strconv.ParseUint(right[i], 10, 64)
		switch {
		case leftErr == nil && rightErr == nil:
			return leftNumber < rightNumber
		case leftErr == nil:
			return true
		case rightErr == nil:
			return false
		default:
			return left[i] < right[i]
		}
	}
	return len(left) < len(right)
}

// parseSemanticVersion finds a SemVer token in human-readable CLI output such
// as "baseten version v0.3.0". It accepts prerelease and build metadata and
// deliberately rejects shortened versions.
func parseSemanticVersion(output string) (semanticVersion, error) {
	for _, field := range strings.Fields(output) {
		candidate := strings.Trim(field, " \t\r\n'\"=,;:()[]{}")
		candidate = strings.TrimPrefix(candidate, "v")
		coreAndPre, build, hasBuild := strings.Cut(candidate, "+")
		core, prerelease, hasPrerelease := strings.Cut(coreAndPre, "-")
		parts := strings.Split(core, ".")
		if len(parts) != 3 {
			continue
		}
		values := make([]uint64, 3)
		valid := true
		for i, part := range parts {
			if part == "" || (len(part) > 1 && part[0] == '0') {
				valid = false
				break
			}
			value, err := strconv.ParseUint(part, 10, 64)
			if err != nil {
				valid = false
				break
			}
			values[i] = value
		}
		if !valid ||
			(hasPrerelease && !validSemVerIdentifiers(prerelease, true)) ||
			(hasBuild && !validSemVerIdentifiers(build, false)) {
			continue
		}
		return semanticVersion{
			major:      values[0],
			minor:      values[1],
			patch:      values[2],
			prerelease: prerelease,
		}, nil
	}
	return semanticVersion{}, errors.New("semantic version not found")
}

func validSemVerIdentifiers(value string, rejectNumericLeadingZero bool) bool {
	if value == "" {
		return false
	}
	for _, identifier := range strings.Split(value, ".") {
		if identifier == "" {
			return false
		}
		numeric := true
		for _, r := range identifier {
			if (r < '0' || r > '9') && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && r != '-' {
				return false
			}
			if r < '0' || r > '9' {
				numeric = false
			}
		}
		if rejectNumericLeadingZero && numeric && len(identifier) > 1 && identifier[0] == '0' {
			return false
		}
	}
	return true
}
