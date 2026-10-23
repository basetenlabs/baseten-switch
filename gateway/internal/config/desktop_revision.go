package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// DesktopModelRevision identifies the complete ordered alias and target list.
func DesktopModelRevision(aliases []string, targets map[string]string) string {
	pairs := make([][2]string, 0, len(aliases))
	for _, alias := range aliases {
		pairs = append(pairs, [2]string{alias, targets[alias]})
	}
	payload, _ := json.Marshal(pairs)
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:])
}
