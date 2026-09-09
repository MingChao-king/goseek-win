package httpapi

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestEnsureBuiltinPluginsUpdatesBinAndPreservesSkill(t *testing.T) {
	previous := BuiltinPluginsFS
	t.Cleanup(func() { BuiltinPluginsFS = previous })
	BuiltinPluginsFS = fstest.MapFS{
		"plugins/browser/bin/goseek-browser": {
			Data: []byte("new binary"),
			Mode: 0o755,
		},
		"plugins/browser/skills/browser/SKILL.md": {
			Data: []byte("embedded skill"),
		},
	}

	root := filepath.Join(t.TempDir(), "plugins")
	binPath := filepath.Join(root, "browser", "bin", "goseek-browser")
	skillPath := filepath.Join(root, "browser", "skills", "browser", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(skillPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binPath, []byte("old binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillPath, []byte("user skill"), 0o644); err != nil {
		t.Fatal(err)
	}

	ensureBuiltinPlugins("", root)

	assertFileContent(t, binPath, "new binary")
	assertFileContent(t, skillPath, "user skill")
	info, err := os.Stat(binPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != fs.FileMode(0o755) {
		t.Fatalf("bin mode = %o", info.Mode().Perm())
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
}
