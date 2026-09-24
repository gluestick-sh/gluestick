package install

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gluestick-sh/core/manifest"
)

func directoryLinkTarget(path string) (target string, ok bool, err error) {
	target, err = os.Readlink(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		if isNotDirectoryLinkErr(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return target, true, nil
}

func isNotDirectoryLinkErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "reparse point") ||
		strings.Contains(msg, "not a symlink") ||
		strings.Contains(msg, "invalid argument")
}

// prepareInstallDirForPreInstallHooks removes stale persist placeholders left by a
// failed install so Scoop-style pre_install New-Item calls can run again.
// Non-empty directories from a fresh extract are kept (e.g. obs-studio data/).
func prepareInstallDirForPreInstallHooks(installDir, persistDir string, persist []manifest.PersistEntry) error {
	if len(persist) == 0 {
		return nil
	}
	if err := os.MkdirAll(persistDir, 0755); err != nil {
		return fmt.Errorf("create persist dir: %w", err)
	}
	for _, item := range persist {
		persistPath := filepath.Join(persistDir, filepath.FromSlash(item.DataName()))
		installPath := filepath.Join(installDir, filepath.FromSlash(item.InstallName()))
		if _, err := os.Stat(persistPath); err == nil {
			continue
		}
		if err := removeStalePersistInstallPath(installPath, item.LooksLikeFile()); err != nil {
			return fmt.Errorf("remove stale %s: %w", item.InstallName(), err)
		}
	}
	return nil
}

// prepareInstallDirForPostInstallHooks ensures persist install paths are directories
// (not stale files) when data is not yet stored under persist/.
func prepareInstallDirForPostInstallHooks(installDir, persistDir string, persist []manifest.PersistEntry) error {
	if len(persist) == 0 {
		return nil
	}
	for _, item := range persist {
		if item.LooksLikeFile() {
			continue
		}
		persistPath := filepath.Join(persistDir, filepath.FromSlash(item.DataName()))
		installPath := filepath.Join(installDir, filepath.FromSlash(item.InstallName()))
		if _, err := os.Stat(persistPath); err == nil {
			continue
		}
		if err := ensureInstallPersistDirectory(installPath); err != nil {
			return fmt.Errorf("prepare %s: %w", item.InstallName(), err)
		}
	}
	return nil
}

// restorePersistOnInstall links install persist paths to persist/ when data exists,
// otherwise ensures empty directories for Scoop post_install hooks.
func restorePersistOnInstall(installDir, persistDir string, persist []manifest.PersistEntry) error {
	if len(persist) == 0 {
		return nil
	}
	if err := os.MkdirAll(persistDir, 0755); err != nil {
		return fmt.Errorf("create persist dir: %w", err)
	}
	for _, item := range persist {
		installPath := filepath.Join(installDir, filepath.FromSlash(item.InstallName()))
		persistPath := filepath.Join(persistDir, filepath.FromSlash(item.DataName()))
		if _, err := os.Stat(persistPath); err == nil {
			if item.LooksLikeFile() {
				if err := normalizePersistFileStore(persistPath); err != nil {
					return fmt.Errorf("normalize persist file %s: %w", item.InstallName(), err)
				}
				if _, err := os.Stat(persistPath); err == nil {
					if err := copyPersistFileToInstall(persistPath, installPath); err != nil {
						return fmt.Errorf("restore persist file %s: %w", item.InstallName(), err)
					}
				}
			} else if err := linkDirectoryJunction(installPath, persistPath); err != nil {
				return fmt.Errorf("link persist %s: %w", item.InstallName(), err)
			}
			continue
		}
		if item.LooksLikeFile() {
			continue
		}
		if err := ensureInstallPersistDirectory(installPath); err != nil {
			return fmt.Errorf("ensure %s: %w", item.InstallName(), err)
		}
	}
	return nil
}

// SavePersistOnUninstall moves version-local persist data into persist/ before the
// version directory is removed. Junctions into persist/ are dropped without deleting data.
func SavePersistOnUninstall(installDir, persistDir string, persist []manifest.PersistEntry) error {
	if len(persist) == 0 {
		return nil
	}
	if err := os.MkdirAll(persistDir, 0755); err != nil {
		return fmt.Errorf("create persist dir: %w", err)
	}
	for _, item := range persist {
		installPath := filepath.Join(installDir, filepath.FromSlash(item.InstallName()))
		persistPath := filepath.Join(persistDir, filepath.FromSlash(item.DataName()))
		if _, err := os.Lstat(installPath); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if target, linked, err := directoryLinkTarget(installPath); err != nil {
			return err
		} else if linked {
			absTarget, absErr := filepath.Abs(target)
			absPersist, persistAbsErr := filepath.Abs(persistPath)
			if absErr == nil && persistAbsErr == nil && strings.EqualFold(absTarget, absPersist) {
				if err := removeDirectoryLink(installPath); err != nil {
					return fmt.Errorf("remove persist link %s: %w", item.InstallName(), err)
				}
				continue
			}
		}
		if err := moveInstallPersistToStore(installPath, persistPath); err != nil {
			return fmt.Errorf("save persist %s: %w", item.InstallName(), err)
		}
	}
	return nil
}

func moveInstallPersistToStore(installPath, persistPath string) error {
	installInfo, err := os.Lstat(installPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !installInfo.IsDir() {
		return mergeInstallFileIntoPersist(installPath, persistPath)
	}
	persistInfo, err := os.Lstat(persistPath)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(persistPath), 0755); err != nil {
			return err
		}
		return os.Rename(installPath, persistPath)
	}
	if err != nil {
		return err
	}
	if !persistInfo.IsDir() {
		return mergeInstallFileIntoPersist(installPath, persistPath)
	}
	entries, err := os.ReadDir(installPath)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		src := filepath.Join(installPath, entry.Name())
		dst := filepath.Join(persistPath, entry.Name())
		if err := mergeInstallEntryIntoPersist(src, dst); err != nil {
			return err
		}
	}
	return removeInstallPersistPath(installPath)
}

func mergeInstallFileIntoPersist(installPath, persistPath string) error {
	if err := os.MkdirAll(filepath.Dir(persistPath), 0755); err != nil {
		return err
	}
	installInfo, err := os.Lstat(installPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if installInfo.IsDir() {
		return fmt.Errorf("expected file at %s", installPath)
	}
	persistInfo, err := os.Lstat(persistPath)
	if os.IsNotExist(err) {
		return os.Rename(installPath, persistPath)
	}
	if err != nil {
		return err
	}
	if persistInfo.IsDir() {
		if err := RemoveAll(persistPath); err != nil {
			return err
		}
		return os.Rename(installPath, persistPath)
	}
	data, err := os.ReadFile(installPath)
	if err != nil {
		return err
	}
	if err := os.WriteFile(persistPath, data, 0644); err != nil {
		return err
	}
	return removeInstallFile(installPath)
}

func mergeInstallEntryIntoPersist(src, dst string) error {
	srcInfo, err := os.Lstat(src)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := os.Stat(dst); os.IsNotExist(err) {
		return os.Rename(src, dst)
	} else if err != nil {
		return err
	}
	if srcInfo.IsDir() {
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := mergeInstallEntryIntoPersist(
				filepath.Join(src, entry.Name()),
				filepath.Join(dst, entry.Name()),
			); err != nil {
				return err
			}
		}
		return removeInstallPersistPath(src)
	}
	return os.Rename(src, dst)
}

func ensureInstallPersistDirectory(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return os.MkdirAll(path, 0755)
	}
	if err != nil {
		return err
	}
	if target, linked, linkErr := directoryLinkTarget(path); linkErr != nil {
		return linkErr
	} else if linked {
		_ = target
		return nil
	}
	if !info.IsDir() {
		if err := os.Remove(path); err != nil {
			return err
		}
		return os.MkdirAll(path, 0755)
	}
	return nil
}

func removeInstallPersistPath(path string) error {
	if target, linked, err := directoryLinkTarget(path); err != nil {
		return err
	} else if linked {
		_ = target
		return removeDirectoryLink(path)
	}
	return RemoveAll(path)
}

// removeStalePersistInstallPath clears empty placeholders from a failed install.
// It does not remove non-empty directories — those are normal extract output.
func removeStalePersistInstallPath(path string, fileEntry bool) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if target, linked, linkErr := directoryLinkTarget(path); linkErr != nil {
		return linkErr
	} else if linked {
		_ = target
		return removeDirectoryLink(path)
	}
	if !info.IsDir() {
		return removeInstallFile(path)
	}
	if fileEntry {
		return RemoveAll(path)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return os.Remove(path)
	}
	return nil
}

func normalizePersistFileStore(persistPath string) error {
	info, err := os.Lstat(persistPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		return RemoveAll(persistPath)
	}
	return nil
}

func copyPersistFileToInstall(persistPath, installPath string) error {
	if err := os.MkdirAll(filepath.Dir(installPath), 0755); err != nil {
		return err
	}
	data, err := os.ReadFile(persistPath)
	if err != nil {
		return err
	}
	if err := removeInstallFile(installPath); err != nil {
		return err
	}
	return os.WriteFile(installPath, data, 0644)
}

func removeInstallFile(path string) error {
	if err := chmodWritable(path); err != nil {
		return err
	}
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func chmodWritable(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0200 != 0 {
		return nil
	}
	return os.Chmod(path, info.Mode()|0200)
}
