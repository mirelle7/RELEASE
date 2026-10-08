package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// GitFunc runs git in dir and returns its trimmed output.
type GitFunc func(ctx context.Context, dir string, args ...string) (string, error)

func realGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0") // never sit waiting for a password
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return text, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, text)
	}
	return text, nil
}

// runRange builds one configuration at many points in history.
//
// Your checkout is never touched: every step is built in a temporary git worktree (so your branch, your working
// files and your index stay exactly as they are). The only things written to your repository are the fetched
// objects and, while the range runs, the worktree's bookkeeping, which is removed at the end.
func (a *Agent) runRange(ctx context.Context, j Job) {
	r := j.Range
	points := r.Points()
	group := j.ID
	label := func(n int) (string, string) {
		if r.Unit == "pr" {
			return fmt.Sprintf("%s-pr%d", group, n), fmt.Sprintf("PR #%d", n)
		}
		return fmt.Sprintf("%s-c%d", group, n), fmt.Sprintf("commit #%d", n)
	}

	if a.DryRun {
		for i, n := range points {
			id, lab := label(n)
			a.setCurrent(id, fmt.Sprintf("%s (%d of %d)", lab, i+1, len(points)))
			a.record(a.build(ctx, a.Src, j, id, &stepMeta{Group: group, Label: lab}))
		}
		return
	}

	git := a.Git
	if git == nil {
		if _, err := exec.LookPath("git"); err != nil {
			a.record(a.fail(j.ID, nil, "git was not found on PATH, and range builds need it."))
			return
		}
		git = realGit
	}
	remote := a.Remote
	if remote == "" {
		remote = "origin"
	}
	if _, err := git(ctx, a.Src, "rev-parse", "--git-dir"); err != nil {
		a.record(a.fail(j.ID, nil, "your game folder is not a git checkout, and range builds need one."))
		return
	}

	var commits []string
	if r.Unit == "commit" {
		out, err := git(ctx, a.Src, "rev-list", "--first-parent", "--reverse", "HEAD")
		if err != nil {
			a.record(a.fail(j.ID, nil, err.Error()))
			return
		}
		commits = strings.Fields(out)
	}

	wt := filepath.Join(a.Out, "worktrees", group)
	_ = os.RemoveAll(wt)
	_, _ = git(ctx, a.Src, "worktree", "prune")
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		a.record(a.fail(j.ID, nil, err.Error()))
		return
	}
	if _, err := git(ctx, a.Src, "worktree", "add", "--detach", wt, "HEAD"); err != nil {
		a.record(a.fail(j.ID, nil, "could not create a temporary worktree: "+err.Error()))
		return
	}
	defer func() {
		bg := context.Background()
		_, _ = git(bg, a.Src, "worktree", "remove", "--force", wt)
		_ = os.RemoveAll(wt)
		_, _ = git(bg, a.Src, "worktree", "prune")
	}()

	for i, n := range points {
		if ctx.Err() != nil {
			break
		}
		id, lab := label(n)
		meta := &stepMeta{Group: group, Label: lab}
		a.setCurrent(id, fmt.Sprintf("%s (%d of %d)", lab, i+1, len(points)))

		sha, err := a.resolveRef(ctx, git, remote, r.Unit, n, commits)
		if err != nil {
			a.record(a.fail(id, meta, err.Error()))
			continue
		}
		meta.Sha = sha
		if _, err := git(ctx, wt, "checkout", "--detach", "--force", sha); err != nil {
			a.record(a.fail(id, meta, err.Error()))
			continue
		}
		if err := presetExists(wt, j.Preset); err != nil {
			a.record(a.fail(id, meta, err.Error()))
			continue
		}
		a.record(a.build(ctx, wt, j, id, meta))
	}
}

func (a *Agent) resolveRef(ctx context.Context, git GitFunc, remote, unit string, n int, commits []string) (string, error) {
	if unit == "commit" {
		if n > len(commits) {
			return "", fmt.Errorf("your history has only %d commits", len(commits))
		}
		return commits[n-1], nil
	}
	if _, err := git(ctx, a.Src, "fetch", "--no-tags", remote, fmt.Sprintf("pull/%d/head", n)); err != nil {
		if ctx.Err() != nil {
			return "", errors.New("cancelled")
		}
		return "", fmt.Errorf("PR #%d could not be fetched from %q (not a pull request there, or no access)", n, remote)
	}
	return git(ctx, a.Src, "rev-parse", "FETCH_HEAD")
}

// presetExists checks that this revision of the project defines the preset (old revisions may predate presets).
func presetExists(dir, preset string) error {
	raw, err := os.ReadFile(filepath.Join(dir, "CMakePresets.json"))
	if err != nil {
		return errors.New("that revision has no CMakePresets.json (it predates the presets)")
	}
	var doc struct {
		Configure []struct {
			Name string `json:"name"`
		} `json:"configurePresets"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return errors.New("that revision's CMakePresets.json is not valid JSON")
	}
	for _, p := range doc.Configure {
		if p.Name == preset {
			return nil
		}
	}
	return fmt.Errorf("that revision has no preset %q", preset)
}
