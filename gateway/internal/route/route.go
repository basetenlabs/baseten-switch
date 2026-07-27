package route

import (
	"errors"
	"os"
	"path/filepath"
)

var Routes = []string{"baseten", "anthropic", "openai", "monitor"}

func Valid(r string) bool {
	for _, v := range Routes {
		if v == r {
			return true
		}
	}
	return false
}

func WriteTo(p, r string) error {
	if !Valid(r) {
		return errors.New("invalid route: " + r)
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(r+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
