package gateway

import "github.com/basetenlabs/baseten-switch/gateway/internal/config"

// ValidateConfigFile runs the same resolver construction used by a live
// gateway reload. CLI config mutations use it before replacing gateway.yaml
// so an offline edit cannot leave a file that fails at the next router start.
func ValidateConfigFile(file *config.File) error {
	_, err := resolveFromFile(file)
	return err
}
