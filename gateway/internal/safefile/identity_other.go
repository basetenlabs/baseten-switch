//go:build !darwin && !linux

package safefile

import (
	"errors"
	"os"
)

const platformSupportsIdentity = false

func identityFromInfo(os.FileInfo) (Identity, error) {
	return Identity{}, errors.New("file identity is unsupported")
}
