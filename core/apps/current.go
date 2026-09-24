package apps

import (
	"fmt"
	"os"
	"path/filepath"
)

// CurrentLinkName is the directory junction/symlink alias for the active version.
const CurrentLinkName = "current"

// InstallRecordFile is persisted under each version directory for reset.
const InstallRecordFile = "install.json"

// PkgRoot returns apps/<pkgName> under glue root.
func PkgRoot(glueRoot, pkgName string) string {
	return filepath.Join(glueRoot, "apps", pkgName)
}

// CurrentInstalledPath returns the current installation directory and version for a package.
// If the package is not installed, returns an error.
func CurrentInstalledPath(glueRoot, pkgName string) (string, string, error) {
	pkgRoot := PkgRoot(glueRoot, pkgName)
	current := filepath.Join(pkgRoot, CurrentLinkName)

	// Check if current junction exists
	if _, err := os.Lstat(current); err != nil {
		return "", "", fmt.Errorf("package not installed: %s", pkgName)
	}

	// Read the current version
	version, err := ReadCurrent(pkgRoot)
	if err != nil {
		return "", "", fmt.Errorf("failed to read current version: %w", err)
	}

	return pkgRoot, version, nil
}

// RemoveCurrent deletes the current junction/symlink without removing the target version directory.
func RemoveCurrent(pkgRoot string) error {
	current := filepath.Join(pkgRoot, CurrentLinkName)
	return removeCurrentLink(current)
}
