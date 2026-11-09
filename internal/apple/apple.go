// Package apple controls macOS apps (Notes, Music) with JavaScript for
// Automation via osascript. User text is always passed as script arguments,
// never spliced into the script source, so a note title can't inject code.
//
// The first use triggers macOS's Automation permission prompt for the app
// voc was started from (Terminal, iTerm…); the user decides there.
package apple

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Runner runs a JXA script with arguments and returns its stdout.
type Runner interface {
	Run(ctx context.Context, script string, args ...string) ([]byte, error)
}

// Osascript runs scripts with /usr/bin/osascript.
type Osascript struct{}

// ErrNotAllowed means macOS refused Automation access.
var ErrNotAllowed = errors.New("macOS hasn't allowed voc to control this app yet")

func (Osascript) Run(ctx context.Context, script string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/osascript", append([]string{"-l", "JavaScript", "-e", script}, args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "-1743") || strings.Contains(msg, "Not authorized") || strings.Contains(msg, "not allowed") {
			return nil, ErrNotAllowed
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("the app took too long to answer")
		}
		// JXA errors look like "execution error: Error: <message> (-2700)".
		if i := strings.Index(msg, "Error: "); i >= 0 {
			msg = msg[i+len("Error: "):]
		}
		return nil, errors.New(strings.TrimSuffix(strings.TrimSpace(msg), " (-2700)"))
	}
	return out, nil
}

// Available reports whether an app is installed.
func Available(app string) bool {
	_, err := os.Stat("/System/Applications/" + app + ".app")
	return err == nil
}

func runJSON[T any](ctx context.Context, r Runner, script string, args ...string) (T, error) {
	var out T
	b, err := r.Run(ctx, script, args...)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return out, fmt.Errorf("unexpected reply from the app: %v", err)
	}
	return out, nil
}

// PermissionHint explains how to fix ErrNotAllowed.
func PermissionHint(app string) string {
	return "voc isn't allowed to control " + app + " yet. Open System Settings → Privacy & Security → Automation and allow it for the app you started voc from (for example Terminal), then try again."
}
