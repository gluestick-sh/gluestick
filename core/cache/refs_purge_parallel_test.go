package cache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gluestick-sh/core/store"
)

func TestListPurgeScanPackages_secondaryExcludesPrimary(t *testing.T) {
	root := testRoot(t)
	appsDir := filepath.Join(root, "apps")
	for _, name := range []string{"nodejs", "git", "freecad"} {
		if err := os.MkdirAll(filepath.Join(appsDir, name, "1.0.0"), 0755); err != nil {
			t.Fatal(err)
		}
	}

	primary := map[string]struct{}{"nodejs": {}, "git": {}}
	got, err := listPurgeScanPackages(appsDir, "nodejs", nil, primary)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "freecad" {
		t.Fatalf("secondary packages = %v, want [freecad]", got)
	}
}

func TestAppsReferenceCandidateHashes_secondaryParallel(t *testing.T) {
	root := testRoot(t)
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	appsDir := filepath.Join(root, "apps")
	orphanHash, err := store.Write(strings.NewReader("only-in-node"))
	if err != nil {
		t.Fatal(err)
	}
	sharedHash, err := store.Write(strings.NewReader("shared-lib"))
	if err != nil {
		t.Fatal(err)
	}

	nodeInstall := filepath.Join(appsDir, "nodejs", "20.0.0")
	freeInstall := filepath.Join(appsDir, "freecad", "1.0.0")
	for _, dir := range []string{nodeInstall, freeInstall} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Link(orphanHash, filepath.Join(nodeInstall, "orphan.dll")); err != nil {
		t.Fatal(err)
	}
	if err := store.Link(sharedHash, filepath.Join(freeInstall, "lib.dll")); err != nil {
		t.Fatal(err)
	}

	refs, err := AppsReferenceCandidateHashes(
		appsDir, store, nil, "nodejs",
		[]string{orphanHash, sharedHash},
		nil, nil, purgeScanBudget{}, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !refs[orphanHash] {
		t.Fatal("expected orphan hash in nodejs install")
	}
	if !refs[sharedHash] {
		t.Fatal("expected shared hash found in freecad during secondary scan")
	}
}
