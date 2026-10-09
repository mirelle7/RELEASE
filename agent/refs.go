package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// gitFunc is the git to use: the injected one in tests, else the real one (an error when git is not installed).
func (a *Agent) gitFunc() (GitFunc, error) {
	if a.Git != nil {
		return a.Git, nil
	}
	if which(a.env, "git") == "" {
		return nil, fmt.Errorf("git was not found on PATH. Install it (winget install --id Git.Git -e, or your package manager) and open a new prompt")
	}
	return a.realGit, nil
}

func (a *Agent) remoteName() string {
	if a.Remote == "" {
		return "origin"
	}
	return a.Remote
}

// branchCandidates are the names to try for a repository's default branch, best first: the remote's HEAD, then main, master, trunk.
func (a *Agent) branchCandidates(ctx context.Context, git GitFunc, src, remote string) []string {
	var out []string
	add := func(b string) {
		if b == "" {
			return
		}
		for _, o := range out {
			if o == b {
				return
			}
		}
		out = append(out, b)
	}
	if ref, err := git(ctx, src, "symbolic-ref", "--short", "refs/remotes/"+remote+"/HEAD"); err == nil {
		add(strings.TrimPrefix(strings.TrimSpace(ref), remote+"/"))
	}
	for _, b := range []string{"main", "master", "trunk"} {
		add(b)
	}
	return out
}

// fetchLatest fetches the default branch and returns its branch name and the commit.
func (a *Agent) fetchLatest(ctx context.Context, git GitFunc, src, remote string) (branch, sha string, err error) {
	var last error
	for _, b := range a.branchCandidates(ctx, git, src, remote) {
		if _, err := git(ctx, src, "fetch", "--no-tags", remote, b); err != nil {
			last = err
			if ctx.Err() != nil {
				break
			}
			continue
		}
		sha, err := git(ctx, src, "rev-parse", "FETCH_HEAD^{commit}")
		if err != nil {
			return "", "", err
		}
		return b, sha, nil
	}
	return "", "", fmt.Errorf("could not fetch the latest version from remote %q (tried its default branch, main, master and trunk): %v", remote, last)
}

// resolveCommit makes sure the commit exists locally (fetching if needed) and returns its full sha.
func (a *Agent) resolveCommit(ctx context.Context, git GitFunc, src, remote, ref string) (string, error) {
	have := func() (string, bool) {
		if _, err := git(ctx, src, "cat-file", "-e", ref+"^{commit}"); err != nil {
			return "", false
		}
		full, err := git(ctx, src, "rev-parse", ref+"^{commit}")
		return full, err == nil
	}
	if full, ok := have(); ok {
		return full, nil
	}
	if _, err := git(ctx, src, "fetch", "--no-tags", remote, ref); err != nil && ctx.Err() == nil {
		_, _ = git(ctx, src, "fetch", "--no-tags", remote)
	}
	if full, ok := have(); ok {
		return full, nil
	}
	return "", fmt.Errorf("commit %s does not exist in this project's repository, even after fetching from %q", ref, remote)
}

// runRef builds a commit or the latest default branch in a temporary worktree; the checkout itself is never touched.
func (a *Agent) runRef(ctx context.Context, j Job) {
	fail := func(meta *stepMeta, msg string) { a.record(a.fail(j.ID, meta, msg)) }
	src, err := a.checkSrc(j.Project)
	if err != nil {
		fail(nil, err.Error())
		return
	}
	git, err := a.gitFunc()
	if err != nil {
		fail(nil, "building a specific version needs git: "+err.Error())
		return
	}
	remote := a.remoteName()
	if _, err := git(ctx, src, "rev-parse", "--git-dir"); err != nil {
		fail(nil, fmt.Sprintf("the folder of project %q is not a git checkout, and building a specific version needs one.", normProject(j.Project)))
		return
	}
	var sha, label string
	if j.Ref == "latest" {
		a.setCurrent(j.ID, "fetching the latest version")
		branch, s, err := a.fetchLatest(ctx, git, src, remote)
		if err != nil {
			fail(nil, err.Error())
			return
		}
		sha, label = s, "latest "+branch
	} else {
		a.setCurrent(j.ID, "looking for commit "+j.Ref)
		s, err := a.resolveCommit(ctx, git, src, remote, j.Ref)
		if err != nil {
			fail(nil, err.Error())
			return
		}
		sha, label = s, shortSha(s)
	}
	meta := &stepMeta{Label: label, Sha: sha}

	wt := filepath.Join(a.Out, "worktrees", j.ID)
	_ = os.RemoveAll(wt)
	_, _ = git(ctx, src, "worktree", "prune")
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		fail(meta, err.Error())
		return
	}
	if _, err := git(ctx, src, "worktree", "add", "--detach", wt, sha); err != nil {
		fail(meta, "could not create a temporary worktree: "+err.Error())
		return
	}
	defer func() {
		bg := context.Background()
		_, _ = git(bg, src, "worktree", "remove", "--force", wt)
		_ = os.RemoveAll(wt)
		_, _ = git(bg, src, "worktree", "prune")
	}()
	if err := presetExists(wt, j.Preset); err != nil {
		fail(meta, err.Error())
		return
	}
	a.setCurrent(j.ID, "building "+label)
	a.record(a.build(ctx, wt, j, j.ID, meta))
}

func shortSha(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

type commitInfo struct {
	Sha     string `json:"sha"`
	Short   string `json:"short"`
	Subject string `json:"subject"`
	Date    string `json:"date"`
	Author  string `json:"author"`
}

// handleCommits: GET /api/commits?project=ID&limit=50&fetch=1
func (a *Agent) handleCommits(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	project := q.Get("project")
	if project != "" && !reProject.MatchString(project) {
		writeJSON(w, 400, map[string]string{"error": "bad project id"})
		return
	}
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil || limit < 1 {
		limit = 50
	}
	limit = min(limit, 200)
	res := map[string]any{"branch": "", "head": "", "fetched": false, "commits": []commitInfo{}}
	bad := func(msg string) {
		res["error"] = msg
		writeJSON(w, 200, res)
	}
	ctx := r.Context()
	auto := a.isAuto(project)
	var src string
	if auto {
		src = a.cacheDir(project)
		if !a.cloned(project) {
			// No download yet: ask GitHub for the list; if that fails, download the source and read its history.
			if slug, ok := githubSlug(a.repoURL(project)); ok {
				if branch, commits, err := a.githubCommits(ctx, slug, "", limit); err == nil && len(commits) > 0 {
					res["branch"], res["head"], res["fetched"], res["commits"] = branch, commits[0].Sha, true, commits
					writeJSON(w, 200, res)
					return
				}
			}
			git, err := a.gitFunc()
			if err != nil {
				bad(err.Error())
				return
			}
			if _, err := a.ensureClone(ctx, git, project, io.Discard); err != nil {
				bad(err.Error())
				return
			}
		}
	} else {
		var err error
		if src, err = a.checkSrc(project); err != nil {
			bad(err.Error())
			return
		}
	}
	git, err := a.gitFunc()
	if err != nil {
		bad(err.Error())
		return
	}
	if _, err := git(ctx, src, "rev-parse", "--git-dir"); err != nil {
		bad(fmt.Sprintf("the folder of project %q is not a git checkout", normProject(project)))
		return
	}
	remote := a.remoteName()
	if auto {
		remote = autoRemote
	}
	if q.Get("fetch") == "1" {
		fctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		fargs := []string{"fetch", "--no-tags", remote}
		if auto { // just the default branch: the clone holds the rest of the history already
			if b := a.branchCandidates(ctx, git, src, remote); len(b) > 0 {
				fargs = append(fargs, b[0])
			}
		}
		_, ferr := git(fctx, src, fargs...)
		cancel()
		res["fetched"] = ferr == nil
	}
	ref, branch := "HEAD", ""
	for _, b := range a.branchCandidates(ctx, git, src, remote) {
		if _, err := git(ctx, src, "rev-parse", "--verify", "--quiet", "refs/remotes/"+remote+"/"+b); err == nil {
			ref, branch = remote+"/"+b, b
			break
		}
	}
	if branch == "" {
		branch, _ = git(ctx, src, "rev-parse", "--abbrev-ref", "HEAD")
	}
	res["branch"] = branch
	head, err := git(ctx, src, "rev-parse", ref)
	if err != nil {
		bad("this repository has no commits yet")
		return
	}
	res["head"] = head
	out, err := git(ctx, src, "log", "--first-parent", "-n", strconv.Itoa(limit), "--format=%H%x1f%s%x1f%aI%x1f%an%x1e", ref)
	if err != nil {
		bad(err.Error())
		return
	}
	commits := []commitInfo{}
	for _, rec := range strings.Split(out, "\x1e") {
		f := strings.Split(strings.TrimSpace(rec), "\x1f")
		if len(f) < 4 {
			continue
		}
		commits = append(commits, commitInfo{Sha: f[0], Short: shortSha(f[0]), Subject: f[1], Date: f[2], Author: f[3]})
	}
	res["commits"] = commits
	writeJSON(w, 200, res)
}
