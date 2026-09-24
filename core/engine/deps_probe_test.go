package engine

import (
	"github.com/gluestick-sh/core/engine/internal/install"

	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gluestick-sh/core/message"
)

func TestProbeSevenZipBootstrapPath(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	sevenZ := filepath.Join(binDir, "7z.exe")
	if err := os.WriteFile(sevenZ, []byte("stub"), 0755); err != nil {
		t.Fatal(err)
	}

	got := ProbeSevenZip(root)
	if !got.OK || got.Path != sevenZ {
		t.Fatalf("ProbeSevenZip() = %+v, want path %q", got, sevenZ)
	}
	if !got.FromBootstrap {
		t.Fatal("expected FromBootstrap")
	}
}

func TestBootstrappedGitPath(t *testing.T) {
	root := t.TempDir()
	want := filepath.Join(root, "bin", "git", "mingw64", "bin", "git.exe")
	if got := install.BootstrappedGitPath(root); got != want {
		t.Fatalf("BootstrappedGitPath() = %q, want %q", got, want)
	}
}

func TestWriteStartupToolNotesOnlyMissing(t *testing.T) {
	root := t.TempDir()
	var buf strings.Builder
	WriteStartupToolNotes(&buf, root)
	out := buf.String()

	for _, id := range StartupToolCheckIDs() {
		spec := toolCheckSpecs[id]
		wantLine := "Note: " + message.FormatEN(spec.startupNote, nil) + "\n"
		if spec.probe(root).OK {
			if strings.Contains(out, wantLine) {
				t.Fatalf("unexpected note for available tool %q: %q", id, out)
			}
			continue
		}
		if !strings.Contains(out, wantLine) {
			t.Fatalf("missing %q in output %q", wantLine, out)
		}
	}
}

func TestCheckToolForDoctorBootstrapPending(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("bootstrap-pending doctor OK is Windows-only")
	}
	root := t.TempDir()
	if ProbeSevenZip(root).OK {
		t.Skip("7z already available")
	}
	doctorCheck, ok := CheckToolForDoctor(message.DoctorCheckSevenZip, root)
	if !ok {
		t.Fatal("expected seven_zip spec")
	}
	if !doctorCheck.OK || doctorCheck.DetailKey != message.DoctorSevenZipWillBootstrap {
		t.Fatalf("doctor check = %+v, want will_bootstrap OK", doctorCheck)
	}
	strict, _ := CheckTool(message.DoctorCheckSevenZip, root)
	if strict.OK {
		t.Fatal("strict CheckTool should not pass when 7z is missing")
	}
}
