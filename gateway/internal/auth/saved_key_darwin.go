package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const savedAPIKeyEncoding = "switch-api-key-v1:"

type macOSKeychain struct {
	path    string
	timeout time.Duration
}

func platformSavedAPIKeyStore() savedAPIKeyStore {
	return keychainSavedAPIKeyStore{keychain: macOSKeychain{}}
}

// security's interactive mode keeps the secret off the process argument list.
// Bound every invocation because locked Keychains can request GUI interaction.
func (k macOSKeychain) run(input string, args ...string) ([]byte, error) {
	timeout := k.timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/security", args...)
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	cmd.WaitDelay = time.Second
	output, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, errors.New("Keychain access timed out")
	}
	if err != nil {
		var exit *exec.ExitError
		// security returns errSecItemNotFound (-25300) as process status 44.
		if errors.As(err, &exit) && exit.ExitCode() == 44 {
			return nil, errSavedAPIKeyNotFound
		}
		return nil, errors.New("Keychain access failed")
	}
	return output, nil
}

func (k macOSKeychain) Get(account string) (string, error) {
	args := []string{"find-generic-password", "-s", savedAPIKeyService, "-a", account, "-w"}
	if k.path != "" {
		args = append(args, k.path)
	}
	output, err := k.run("", args...)
	if err != nil {
		return "", err
	}
	encoded := strings.TrimSpace(string(output))
	if !strings.HasPrefix(encoded, savedAPIKeyEncoding) {
		return "", errors.New("invalid saved Keychain credential")
	}
	value, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(encoded, savedAPIKeyEncoding))
	if err != nil {
		return "", errors.New("invalid saved Keychain credential")
	}
	return string(value), nil
}

func (k macOSKeychain) Set(account, key string) error {
	// Service, account and the encoded payload contain no shell metacharacters.
	command := "add-generic-password -U -s " + savedAPIKeyService + " -a " + account + " -w " + savedAPIKeyEncoding + base64.StdEncoding.EncodeToString([]byte(key))
	if k.path != "" {
		command += " " + strconv.Quote(k.path)
	}
	command += "\n"
	if len(command) >= 4096 {
		return errSavedAPIKeyTooLong
	}
	_, err := k.run(command, "-i")
	return err
}

func (k macOSKeychain) Delete(account string) error {
	args := []string{"delete-generic-password", "-s", savedAPIKeyService, "-a", account}
	if k.path != "" {
		args = append(args, k.path)
	}
	_, err := k.run("", args...)
	return err
}
