package install

import (
	"testing"

	"github.com/gluestick-sh/core/cache"
)

func TestShouldExtractFromCache_plainBat(t *testing.T) {
	entry := &cache.PackageEntry{
		Files: map[string]string{"abc": "mill.bat"},
	}
	if ShouldExtractFromCache(".bat", entry, "mill.bat", "", nil, "", "") {
		t.Fatal(".bat cache entry should link, not extract")
	}
}

func TestShouldExtractFromCache_archiveOnly(t *testing.T) {
	entry := &cache.PackageEntry{
		Files: map[string]string{"abc": "tool.tar.gz"},
	}
	if !ShouldExtractFromCache(".tar", entry, "tool.tar.gz", "", nil, "", "") {
		t.Fatal("tar archive-only cache entry should extract")
	}
}

func TestShouldExtractFromCache_multiFileLink(t *testing.T) {
	entry := &cache.PackageEntry{
		Files: map[string]string{
			"a": "tool.zip",
			"b": "tool.exe",
		},
	}
	if ShouldExtractFromCache(".zip", entry, "tool.zip", "", nil, "", "") {
		t.Fatal("multi-file cache entry should link extracted files")
	}
}

func TestShouldExtractFromCache_membersOnlyNoExtract(t *testing.T) {
	entry := &cache.PackageEntry{
		Files: map[string]string{
			"aaa": "yazi.exe",
			"bbb": "ya.exe",
		},
	}
	if ShouldExtractFromCache(".zip", entry, "yazi-x86_64-pc-windows-msvc.zip", "", nil, "", "") {
		t.Fatal("cache with extracted members only should link, not extract")
	}
}

func TestShouldExtractFromCache_zipArchiveBlobByHash(t *testing.T) {
	archiveHash := "9d590a7beb702d6d54678a3f1766e1d7cdc2b9faa577f507c26e76cd625a687d"
	entry := &cache.PackageEntry{
		Files: map[string]string{
			archiveHash: "590a7beb702d6d54678a3f1766e1d7cdc2b9faa577f507c26e76cd625a687d",
		},
	}
	if !ShouldExtractFromCache(".zip", entry, "pixi-x64-pc-windows-msvc.zip", archiveHash, nil, "", "") {
		t.Fatal("zip cache with only archive blob (hash filename) should extract, not link zero files")
	}
}

