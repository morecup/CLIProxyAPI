package helps

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// InspectDesktopWorkspace measures the selected existing directory. An absent
// git executable or an unrelated git failure remains unknown, not false.
func InspectDesktopWorkspace(ctx context.Context, folder string) (string, *bool, error) {
	if !filepath.IsAbs(folder) {
		return "", nil, os.ErrInvalid
	}
	resolved, err := filepath.EvalSymlinks(folder)
	if err != nil {
		return "", nil, err
	}
	stat, err := os.Stat(resolved)
	if err != nil {
		return "", nil, err
	}
	if !stat.IsDir() {
		return "", nil, os.ErrInvalid
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return resolved, nil, nil
	}
	output, err := exec.CommandContext(ctx, git, "-C", resolved, "rev-parse", "--is-inside-work-tree").CombinedOutput()
	if ctx.Err() != nil {
		return "", nil, ctx.Err()
	}
	if err == nil && strings.TrimSpace(string(output)) == "true" {
		yes := true
		return resolved, &yes, nil
	}
	if strings.Contains(string(output), "not a git repository") {
		no := false
		return resolved, &no, nil
	}
	return resolved, nil, nil
}
