package apple

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestScriptsCompile syntax-checks every JXA script with osacompile, which
// compiles without running anything (no Apple events, no permission prompt).
func TestScriptsCompile(t *testing.T) {
	if _, err := exec.LookPath("osacompile"); err != nil {
		t.Skip("not macOS")
	}
	for name, src := range map[string]string{
		"notesSearch": notesSearchJS, "notesRead": notesReadJS, "notesCreate": notesCreateJS,
		"musicStatus": musicStatusJS, "musicControl": musicControlJS, "musicPlay": musicPlayJS,
	} {
		out := filepath.Join(t.TempDir(), name+".scpt")
		if b, err := exec.Command("osacompile", "-l", "JavaScript", "-e", src, "-o", out).CombinedOutput(); err != nil {
			t.Errorf("%s: %v %s", name, err, b)
		}
		os.Remove(out)
	}
}
