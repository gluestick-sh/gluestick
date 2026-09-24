package archmember

import "testing"

func TestIsDirectoryPlaceholder(t *testing.T) {
	tests := []struct {
		name string
		size uint64
		want bool
	}{
		{`runtimes\win-x64\`, 0, true},
		{`Dependencies\luadec\`, 0, true},
		{`AssetStudioGUI.exe`, 100, false},
		{`Dependencies\luadec\lua51\luadec.exe`, 100, false},
	}
	for _, tc := range tests {
		if got := IsDirectoryPlaceholder(tc.name, tc.size); got != tc.want {
			t.Errorf("IsDirectoryPlaceholder(%q, %d) = %v, want %v", tc.name, tc.size, got, tc.want)
		}
	}
}

func TestNormalizeMember(t *testing.T) {
	if got := NormalizeMember(`Dependencies\luadec\lua51\luadec.exe`); got != "Dependencies/luadec/lua51/luadec.exe" {
		t.Fatalf("NormalizeMember = %q", got)
	}
}

func TestSingleRootPrefix(t *testing.T) {
	members := map[string]string{
		"FreeCAD/bin/FreeCAD.exe":    "h1",
		"FreeCAD/bin/FreeCADCmd.exe": "h2",
		"FreeCAD/Mod/":               "h3",
	}
	prefix, ok := SingleRootPrefix(members)
	if !ok || prefix != "FreeCAD/" {
		t.Fatalf("SingleRootPrefix = %q, %v", prefix, ok)
	}
	stripped := StripRootPrefix(members, prefix)
	if stripped["bin/FreeCAD.exe"] != "h1" {
		t.Fatalf("StripRootPrefix = %#v", stripped)
	}
	mixed := map[string]string{
		"a/x": "h1",
		"b/y": "h2",
	}
	if _, ok := SingleRootPrefix(mixed); ok {
		t.Fatal("expected mixed roots to fail")
	}
}
