package install

import "testing"

func TestManifestUsesMultiArtifactURLs_vcredist(t *testing.T) {
	urls := []string{
		"https://aka.ms/vc14/vc_redist.x64.exe",
		"https://aka.ms/vc14/vc_redist.x86.exe",
	}
	hashes := []string{
		"843068991daaa1f73ad9f6239bce4d0f6a07a51f18c37ea2a867e9beca71295c",
		"f0bab33a302b3cdb2e11113760d016f54fd3d2632c65ba7834fac4f0abd7f1a3",
	}
	if !manifestUsesMultiArtifactURLs(urls, hashes) {
		t.Fatal("vcredist URLs should be multi-artifact")
	}
}

func TestManifestUsesMultiArtifactURLs_mirrors(t *testing.T) {
	urls := []string{
		"https://mirror.example/a.zip",
		"https://mirror.example/b.zip",
	}
	hashes := []string{"abc123"}
	if manifestUsesMultiArtifactURLs(urls, hashes) {
		t.Fatal("single hash with multiple URLs should be mirrors, not multi-artifact")
	}
}

func TestExpectedMultiArtifactNames(t *testing.T) {
	pairs := buildURLHashPairs(
		[]string{
			"https://aka.ms/vc14/vc_redist.x64.exe",
			"https://aka.ms/vc14/vc_redist.x86.exe",
		},
		[]string{"deadbeef", "feedface"},
	)
	names, err := expectedMultiArtifactNames(pairs)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "vc_redist.x64.exe" || names[1] != "vc_redist.x86.exe" {
		t.Fatalf("names = %v", names)
	}
}
