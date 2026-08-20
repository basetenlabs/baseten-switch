//go:build darwin || linux

package tracepackage

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type secureDecodeDirectory struct {
	path     string
	file     *os.File
	identity fileIdentity
}

type decodeWorkspace struct {
	parent     *secureDecodeDirectory
	root       *os.Root
	directory  *os.File
	identity   fileIdentity
	stageName  string
	stagePath  string
	outputName string
	published  bool
}

func openSecureDecodeDirectory(value string) (*secureDecodeDirectory, error) {
	abs, err := filepath.Abs(value)
	if err != nil {
		return nil, err
	}
	if err := rejectSymlinkComponents(abs, true); err != nil {
		return nil, err
	}
	before, err := os.Lstat(abs)
	if err != nil || !before.IsDir() || before.Mode()&fs.ModeSymlink != 0 {
		return nil, errors.New("trace package decode: directory must be a non-symlink directory")
	}
	beforeIdentity, err := identityFromFileInfo(before)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open(abs, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("trace package decode: open directory without following links: %w", err)
	}
	file := os.NewFile(uintptr(fd), abs)
	after, statErr := file.Stat()
	current, pathErr := os.Lstat(abs)
	componentErr := rejectSymlinkComponents(abs, true)
	if statErr != nil || pathErr != nil || componentErr != nil || !sameObjectIdentity(after, beforeIdentity) || !os.SameFile(before, current) {
		_ = file.Close()
		return nil, errors.New("trace package decode: directory changed while opening")
	}
	identity, err := identityFromFileInfo(after)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &secureDecodeDirectory{path: filepath.Clean(abs), file: file, identity: identity}, nil
}

func (directory *secureDecodeDirectory) close() error {
	if directory == nil || directory.file == nil {
		return nil
	}
	err := directory.file.Close()
	directory.file = nil
	return err
}

func (directory *secureDecodeDirectory) verifyPath() error {
	current, err := openSecureDecodeDirectory(directory.path)
	if err != nil {
		return err
	}
	defer current.close()
	if !sameObjectIdentityFromIdentities(current.identity, directory.identity) {
		return errors.New("trace package decode: directory path changed")
	}
	return nil
}

func sameObjectIdentityFromIdentities(left, right fileIdentity) bool {
	return left.device == right.device && left.inode == right.inode && left.owner == right.owner && left.mode.Type() == right.mode.Type()
}

func createDecodeWorkspace(outputPath string) (*decodeWorkspace, error) {
	parent, err := openSecureDecodeDirectory(filepath.Dir(outputPath))
	if err != nil {
		return nil, err
	}
	outputName := filepath.Base(outputPath)
	if outputName == "." || outputName == string(filepath.Separator) || outputName == "" {
		_ = parent.close()
		return nil, errors.New("trace package decode: invalid output directory name")
	}
	if err := ensureDecodeChildAbsent(parent, outputName); err != nil {
		_ = parent.close()
		return nil, err
	}

	for attempt := 0; attempt < 32; attempt++ {
		name, err := randomDecodeStageName()
		if err != nil {
			_ = parent.close()
			return nil, err
		}
		if err := unix.Mkdirat(int(parent.file.Fd()), name, 0o700); err != nil {
			if errors.Is(err, unix.EEXIST) {
				continue
			}
			_ = parent.close()
			return nil, fmt.Errorf("trace package decode: create staging directory: %w", err)
		}
		workspace, err := openDecodeWorkspace(parent, name, outputName)
		if err != nil {
			_ = removeDecodeChildDirectory(parent, name, fileIdentity{})
			_ = parent.close()
			return nil, err
		}
		return workspace, nil
	}
	_ = parent.close()
	return nil, errors.New("trace package decode: could not reserve a unique staging directory")
}

func randomDecodeStageName() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("trace package decode: create staging identifier: %w", err)
	}
	return ".baseten-switch-decode-" + hex.EncodeToString(value[:]), nil
}

func openDecodeWorkspace(parent *secureDecodeDirectory, name, outputName string) (*decodeWorkspace, error) {
	fd, err := unix.Openat(int(parent.file.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("trace package decode: open staging directory: %w", err)
	}
	directory := os.NewFile(uintptr(fd), name)
	info, err := directory.Stat()
	if err != nil {
		_ = directory.Close()
		return nil, err
	}
	identity, err := identityFromFileInfo(info)
	if err != nil {
		_ = directory.Close()
		return nil, err
	}
	if err := parent.verifyPath(); err != nil {
		_ = directory.Close()
		return nil, err
	}
	root, err := os.OpenRoot(filepath.Join(parent.path, name))
	if err != nil {
		_ = directory.Close()
		return nil, fmt.Errorf("trace package decode: open staging root: %w", err)
	}
	rootInfo, err := root.Stat(".")
	if err != nil || !sameObjectIdentity(rootInfo, identity) {
		_ = root.Close()
		_ = directory.Close()
		return nil, errors.New("trace package decode: staging directory changed while opening")
	}
	return &decodeWorkspace{
		parent: parent, root: root, directory: directory, identity: identity,
		stageName: name, stagePath: filepath.Join(parent.path, name),
		outputName: outputName,
	}, nil
}

func ensureDecodeChildAbsent(parent *secureDecodeDirectory, name string) error {
	var stat unix.Stat_t
	err := unix.Fstatat(int(parent.file.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if err == nil {
		return ErrDecodeDestinationExists
	}
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	return fmt.Errorf("trace package decode: inspect output directory: %w", err)
}

func (workspace *decodeWorkspace) cleanup() error {
	if workspace == nil {
		return nil
	}
	var result error
	if !workspace.published && workspace.root != nil {
		result = errors.Join(result, removeRootContents(workspace.root))
	}
	if workspace.root != nil {
		result = errors.Join(result, workspace.root.Close())
		workspace.root = nil
	}
	if workspace.directory != nil {
		result = errors.Join(result, workspace.directory.Close())
		workspace.directory = nil
	}
	if !workspace.published {
		result = errors.Join(result, removeDecodeChildDirectory(workspace.parent, workspace.stageName, workspace.identity))
	}
	result = errors.Join(result, workspace.parent.close())
	return result
}

func removeRootContents(root *os.Root) error {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := root.RemoveAll(entry.Name()); err != nil {
			return err
		}
	}
	return nil
}

func removeDecodeChildDirectory(parent *secureDecodeDirectory, name string, expected fileIdentity) error {
	fd, err := unix.Openat(int(parent.file.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return errors.New("trace package decode: refused cleanup after staging identity changed")
	}
	file := os.NewFile(uintptr(fd), name)
	info, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil || closeErr != nil {
		return errors.Join(statErr, closeErr)
	}
	if expected != (fileIdentity{}) && !sameObjectIdentity(info, expected) {
		return errors.New("trace package decode: refused cleanup after staging identity changed")
	}
	if err := unix.Unlinkat(int(parent.file.Fd()), name, unix.AT_REMOVEDIR); err != nil {
		return fmt.Errorf("trace package decode: remove empty staging directory: %w", err)
	}
	return parent.file.Sync()
}

func (workspace *decodeWorkspace) publish() error {
	if workspace == nil || workspace.root == nil || workspace.directory == nil || workspace.published {
		return errors.New("trace package decode: invalid staging publication state")
	}
	if err := workspace.parent.verifyPath(); err != nil {
		return err
	}
	if err := syncDecodedRoot(workspace.root); err != nil {
		return err
	}
	if err := workspace.directory.Sync(); err != nil {
		return fmt.Errorf("trace package decode: sync staging directory: %w", err)
	}
	// Verify again immediately before the descriptor-relative rename so a
	// replaced visible parent path cannot receive an output that was built
	// beneath a different directory object.
	if err := workspace.parent.verifyPath(); err != nil {
		return err
	}
	if err := renameDecodeDirectoryNoReplace(
		int(workspace.parent.file.Fd()), workspace.stageName,
		int(workspace.parent.file.Fd()), workspace.outputName,
	); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return ErrDecodeDestinationExists
		}
		return fmt.Errorf("trace package decode: publish decoded directory: %w", err)
	}
	workspace.published = true
	if err := workspace.parent.file.Sync(); err != nil {
		return fmt.Errorf("trace package decode: sync output parent: %w", err)
	}
	if err := workspace.parent.verifyPath(); err != nil {
		return err
	}
	fd, err := unix.Openat(int(workspace.parent.file.Fd()), workspace.outputName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return errors.New("trace package decode: published directory is unavailable")
	}
	file := os.NewFile(uintptr(fd), workspace.outputName)
	info, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil || closeErr != nil || !sameObjectIdentity(info, workspace.identity) {
		return errors.New("trace package decode: published directory identity changed")
	}
	return nil
}
