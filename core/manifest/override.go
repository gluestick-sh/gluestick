package manifest

import (
	"fmt"
)

// ApplyDownloadOverride returns a manifest copy with download url/hash replaced for installArch.
func ApplyDownloadOverride(m *Manifest, installArch string, urls, hashes []string) (*Manifest, error) {
	if m == nil {
		return nil, fmt.Errorf("manifest is nil")
	}
	if len(urls) == 0 {
		return m, nil
	}
	out, err := cloneManifest(m)
	if err != nil {
		return nil, err
	}
	if err := setDownloadForInstall(out, installArch, urls, hashes); err != nil {
		return nil, err
	}
	return out, nil
}

func setDownloadForInstall(m *Manifest, installArch string, urls, hashes []string) error {
	if installArch != "" && m.Architecture != nil {
		if block, ok := m.Architecture[installArch].(map[string]interface{}); ok {
			setURLHashOnBlock(block, urls, hashes)
			m.Architecture[installArch] = block
			return nil
		}
	}
	setURLHashOnRoot(m, urls, hashes)
	return nil
}

func setURLHashOnRoot(m *Manifest, urls, hashes []string) {
	if len(urls) == 1 {
		m.URL = urls[0]
	} else {
		m.URL = urls
	}
	if len(hashes) == 0 {
		return
	}
	if len(hashes) == 1 {
		m.Hash = hashes[0]
	} else {
		m.Hash = hashes
	}
}

func setURLHashOnBlock(block map[string]interface{}, urls, hashes []string) {
	if len(urls) == 1 {
		block["url"] = urls[0]
	} else {
		block["url"] = urls
	}
	if len(hashes) == 0 {
		return
	}
	if len(hashes) == 1 {
		block["hash"] = hashes[0]
	} else {
		block["hash"] = hashes
	}
}
