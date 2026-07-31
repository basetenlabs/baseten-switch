//go:build darwin || linux

package safefile

import (
	"fmt"
	"os"
	"syscall"
)

const platformSupportsIdentity = true

func identityFromInfo(info os.FileInfo) (Identity, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return Identity{}, fmt.Errorf("unsupported file identity type %T", info.Sys())
	}
	return Identity{
		Device:          uint64(stat.Dev),
		Inode:           uint64(stat.Ino),
		Links:           uint64(stat.Nlink),
		Size:            info.Size(),
		ModTimeUnixNano: info.ModTime().UnixNano(),
	}, nil
}
