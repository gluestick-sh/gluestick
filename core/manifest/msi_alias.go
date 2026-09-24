package manifest

import (
	"path"
	"strings"
)

// IsScoopMsiAlias reports Scoop #/dl.msi_ fragments (MSI expanded in pre_install).
func IsScoopMsiAlias(name string) bool {
	return strings.EqualFold(path.Base(name), "dl.msi_")
}

// IsScoopMsiHookInstall reports installs that link dl.msi_ and run Expand-MsiArchive in pre_install.
func IsScoopMsiHookInstall(localName, fileExt string) bool {
	if IsScoopMsiAlias(localName) {
		return true
	}
	return strings.EqualFold(fileExt, ".msi_")
}
