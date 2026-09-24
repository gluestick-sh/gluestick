package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/gluestick-sh/core/apperr"
)

func TestIsInstallResolveNoticeTyped(t *testing.T) {
	err := &apperr.ManifestNotFound{Name: "foo"}
	if !IsInstallResolveNotice(err) {
		t.Fatal("expected resolve notice")
	}
	if !errors.Is(err, ErrManifestNotFound) {
		t.Fatal("expected ErrManifestNotFound")
	}
}

func TestResolveInstallRef_notFound(t *testing.T) {
	root := t.TempDir()
	eng, err := NewEngine(&EngineConfig{RootDir: root})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	_, err = eng.ResolveInstallRef(context.Background(), "definitely-missing-pkg")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrManifestNotFound) {
		t.Fatalf("expected ErrManifestNotFound, got %v", err)
	}
}
