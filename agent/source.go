package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// A project's source comes from one of two places:
//   - "local": a folder the player pointed the agent at (-src, -project, the page). Used as it is.
//   - "auto" (the default): the agent downloads it from GitHub itself into a cache folder, one blobless clone per
//     project, and checks out the revision each build asks for.

type repoInfo struct{ Name, URL string }

var knownRepos = map[string]repoInfo{
	"ggc":            {"GeneralsGameCode (TheSuperHackers)", "https://github.com/TheSuperHackers/GeneralsGameCode.git"},
	"generalsx":      {"GeneralsX", "https://github.com/fbraz3/GeneralsX.git"},
	"bobtista":       {"GeneralsGameCode (bobtista)", "https://github.com/bobtista/GeneralsGameCode.git"},
	"generalsonline": {"Generals Online", "https://github.com/GeneralsOnlineDevelopmentTeam/GameClient.git"},
}

const autoRemote = "origin"

// repoURL is the git URL of a known project ("" when the project is unknown: it then needs a folder).
func (a *Agent) repoURL(project string) string {
	p := normProject(project)
	if u := a.RepoURLs[p]; u != "" {
		return u
	}
	return knownRepos[p].URL
}

// isAuto: no folder is configured and the agent knows where to download the project from.
func (a *Agent) isAuto(project string) bool {
	return a.srcFor(project) == "" && a.repoURL(project) != ""
}

func (a *Agent) cacheRoot() string {
	if a.CacheDir != "" {
		return a.CacheDir
	}
	if d, err := os.UserCacheDir(); err == nil && d != "" {
		return filepath.Join(d, "gpa")
	}
	return filepath.Join(os.TempDir(), "gpa")
}

// cacheDir is deliberately short (Windows MAX_PATH).
func (a *Agent) cacheDir(project string) string { return filepath.Join(a.cacheRoot(), normProject(project)) }

func (a *Agent) cloned(project string) bool {
	_, err := os.Stat(filepath.Join(a.cacheDir(project), ".git"))
	return err == nil
}

// presetSrc is where to read CMakePresets.json for a project: its folder, or its cached clone when there is one.
func (a *Agent) presetSrc(project string) string {
	if d := a.srcFor(project); d != "" {
		return d
	}
	if a.cloned(project) {
		return a.cacheDir(project)
	}
	return ""
}

func (a *Agent) cloneLock(project string) *sync.Mutex {
	m, _ := a.cloneMus.LoadOrStore(normProject(project), &sync.Mutex{})
	return m.(*sync.Mutex)
}

func (a *Agent) setProgress(msg string) {
	a.mu.Lock()
	a.progress = msg
	a.mu.Unlock()
}

// repoDisplay turns a URL into "github.com/owner/name".
func repoDisplay(url string) string {
	u := strings.TrimSuffix(url, ".git")
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	return u
}

// githubSlug returns owner/name for a github.com URL.
func githubSlug(url string) (string, bool) {
	u := strings.TrimSuffix(url, ".git")
	for _, p := range []string{"https://github.com/", "http://github.com/"} {
		if strings.HasPrefix(u, p) {
			s := strings.TrimPrefix(u, p)
			if strings.Count(s, "/") == 1 {
				return s, true
			}
		}
	}
	return "", false
}

// logGit runs git, copying the command and its output into the build log.
func logGit(ctx context.Context, git GitFunc, log io.Writer, dir string, args ...string) (string, error) {
	fmt.Fprintf(log, "\n$ git %s\n", strings.Join(args, " "))
	out, err := git(ctx, dir, args...)
	if out != "" {
		fmt.Fprintln(log, out)
	} else if err != nil {
		fmt.Fprintln(log, err)
	}
	return out, err
}

func cancelled(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return errors.New("cancelled")
	}
	return err
}

// ensureClone creates the project's blobless clone the first time. Returns the clone folder.
func (a *Agent) ensureClone(ctx context.Context, git GitFunc, project string, log io.Writer) (string, error) {
	mu := a.cloneLock(project)
	mu.Lock()
	defer mu.Unlock()
	dir, url := a.cacheDir(project), a.repoURL(project)
	if a.cloned(project) {
		return dir, nil
	}
	_ = removeAll(dir)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	msg := "Downloading the source from " + repoDisplay(url) + " (first time only)…"
	a.setProgress(msg)
	fmt.Fprintln(log, msg)
	if _, err := logGit(ctx, git, log, filepath.Dir(dir), "clone", "--filter=blob:none", "--no-checkout", "--origin", autoRemote, url, dir); err != nil {
		_ = removeAll(dir)
		return "", cancelled(ctx, fmt.Errorf("could not download the source from %s. Check your internet connection. %v", repoDisplay(url), err))
	}
	_, _ = logGit(ctx, git, log, dir, "config", "core.longpaths", "true")
	return dir, nil
}

// defaultBranch asks the remote what its default branch is (never hard-coded: one fork calls it bobtista/topic/trunk).
func (a *Agent) defaultBranch(ctx context.Context, git GitFunc, dir string) (string, error) {
	out, err := git(ctx, dir, "ls-remote", "--symref", autoRemote, "HEAD")
	if err == nil {
		sc := bufio.NewScanner(strings.NewReader(out))
		for sc.Scan() {
			if rest, ok := strings.CutPrefix(sc.Text(), "ref: refs/heads/"); ok {
				if name, _, ok := strings.Cut(rest, "\t"); ok && strings.TrimSpace(name) != "" {
					return strings.TrimSpace(name), nil
				}
			}
		}
	}
	if ctx.Err() != nil {
		return "", errors.New("cancelled")
	}
	if ref, err2 := git(ctx, dir, "symbolic-ref", "--short", "refs/remotes/"+autoRemote+"/HEAD"); err2 == nil {
		if b := strings.TrimPrefix(strings.TrimSpace(ref), autoRemote+"/"); b != "" {
			return b, nil
		}
	}
	if err == nil {
		err = errors.New("the remote reported no default branch")
	}
	return "", err
}

// fetchTip makes sure the clone exists and fetches the default branch; returns the clone, the branch and its tip.
func (a *Agent) fetchTip(ctx context.Context, git GitFunc, project string, log io.Writer) (dir, branch, sha string, err error) {
	if dir, err = a.ensureClone(ctx, git, project, log); err != nil {
		return
	}
	a.setProgress("Looking for the newest version…")
	if branch, err = a.defaultBranch(ctx, git, dir); err != nil {
		err = cancelled(ctx, fmt.Errorf("could not find out the newest version: %v", err))
		return
	}
	if _, err = logGit(ctx, git, log, dir, "fetch", "--no-tags", autoRemote, branch); err != nil {
		err = cancelled(ctx, err)
		return
	}
	sha, err = logGit(ctx, git, log, dir, "rev-parse", "FETCH_HEAD^{commit}")
	return
}

// checkout puts the cache folder on sha, keeping build/ so rebuilds stay incremental.
func (a *Agent) checkout(ctx context.Context, git GitFunc, dir, sha string, log io.Writer) error {
	msg := "Checking out " + shortSha(sha) + "…"
	a.setProgress(msg)
	fmt.Fprintln(log, "\n"+msg)
	if _, err := logGit(ctx, git, log, dir, "checkout", "--force", "--detach", sha); err != nil {
		return cancelled(ctx, err)
	}
	if _, err := logGit(ctx, git, log, dir, "clean", "-fdx", "-e", "build"); err != nil {
		return cancelled(ctx, err)
	}
	return nil
}

// syncSource brings the project's cache folder to ref ("" or "latest" = the default branch's tip, else a commit sha).
func (a *Agent) syncSource(ctx context.Context, project, ref string, log io.Writer) (dir, sha, label string, err error) {
	git, err := a.gitFunc()
	if err != nil {
		return "", "", "", fmt.Errorf("git is needed to download the source: %v", err)
	}
	if ref == "" || ref == "latest" {
		var branch string
		if dir, branch, sha, err = a.fetchTip(ctx, git, project, log); err != nil {
			return "", "", "", err
		}
		label = "latest " + branch
	} else {
		if dir, err = a.ensureClone(ctx, git, project, log); err != nil {
			return "", "", "", err
		}
		a.setProgress("Looking for commit " + ref + "…")
		if sha, err = a.resolveCommit(ctx, git, dir, autoRemote, ref); err != nil {
			return "", "", "", cancelled(ctx, err)
		}
		label = shortSha(sha)
	}
	if err = a.checkout(ctx, git, dir, sha, log); err != nil {
		return "", "", "", err
	}
	return dir, sha, label, nil
}

// runAuto builds one job from the downloaded source.
func (a *Agent) runAuto(ctx context.Context, j Job) {
	if a.DryRun {
		a.record(a.build(ctx, a.cacheDir(j.Project), j, j.ID, nil))
		return
	}
	logDir := filepath.Join(a.Out, j.ID)
	_ = os.MkdirAll(logDir, 0o755)
	log, err := os.Create(filepath.Join(logDir, "build.log"))
	if err != nil {
		a.record(a.fail(j.ID, nil, err.Error()))
		return
	}
	dir, sha, label, err := a.syncSource(ctx, j.Project, j.Ref, log)
	if err != nil {
		fmt.Fprintln(log, "\nFAILED:", err)
		log.Close()
		a.record(a.fail(j.ID, nil, err.Error()))
		return
	}
	meta := &stepMeta{Label: label, Sha: sha, KeepLog: true}
	if err := presetExists(dir, j.Preset); err != nil {
		fmt.Fprintln(log, "\nFAILED:", err)
		log.Close()
		a.record(a.fail(j.ID, meta, err.Error()))
		return
	}
	log.Close()
	a.setProgress("Building " + label)
	a.record(a.build(ctx, dir, j, j.ID, meta))
}

// ---------------------------------------------------------------------------------------------- info

type sizeEntry struct {
	at time.Time
	mb int
}

// cacheMB is the cache folder's size, cached for 30 s because walking a checkout is slow.
func (a *Agent) cacheMB(project string) int {
	p := normProject(project)
	a.sizeMu.Lock()
	defer a.sizeMu.Unlock()
	if e, ok := a.sizes[p]; ok && time.Since(e.at) < 30*time.Second {
		return e.mb
	}
	var total int64
	_ = filepath.WalkDir(a.cacheDir(p), func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	if a.sizes == nil {
		a.sizes = map[string]sizeEntry{}
	}
	mb := int((total + (1 << 20) - 1) >> 20)
	a.sizes[p] = sizeEntry{time.Now(), mb}
	return mb
}

// cacheHead is the short sha of the cached checkout ("" when none), read from .git/HEAD without running git.
func (a *Agent) cacheHead(project string) string {
	raw, err := os.ReadFile(filepath.Join(a.cacheDir(project), ".git", "HEAD"))
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(raw))
	if reRef.MatchString(s) {
		return shortSha(s)
	}
	return ""
}

// doctorSource adds the "Source" check, and requires git, when the project is downloaded by the agent.
func (a *Agent) doctorSource(rep *Report, project string) {
	if !a.isAuto(project) {
		return
	}
	url := a.repoURL(project)
	if which(a.env, "git") == "" && a.Git == nil {
		have := false
		for _, c := range rep.Checks {
			have = have || c.ID == "git"
		}
		if !have {
			rep.Checks = append(rep.Checks, Check{ID: "git", Name: "Git", Status: "fail", Detail: "Git was not found. The agent uses it to download the game's source.", Fix: wingetHint(a, "Git.Git")})
		}
	}
	c := Check{ID: "source", Name: "Source", Status: "info"}
	if h := a.cacheHead(project); a.cloned(project) && h != "" {
		c.Detail = fmt.Sprintf("cached %s, %d MB (%s)", h, a.cacheMB(project), repoDisplay(url))
	} else if a.cloned(project) {
		c.Detail = fmt.Sprintf("cached, %d MB (%s)", a.cacheMB(project), repoDisplay(url))
	} else {
		c.Detail = "will be downloaded from " + repoDisplay(url) + " on the first build"
	}
	rep.Checks = append(rep.Checks, c)
}

// ---------------------------------------------------------------------------------------------- commits without a folder

// githubCommits asks the GitHub REST API for the newest commits of a github.com repository.
func (a *Agent) githubCommits(ctx context.Context, slug, branch string, limit int) (string, []commitInfo, error) {
	base := a.APIBase
	if base == "" {
		base = "https://api.github.com"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	get := func(path string, into any) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "generals-portal-agent")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("GitHub answered %s", resp.Status)
		}
		return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(into)
	}
	if branch == "" {
		var repo struct {
			Default string `json:"default_branch"`
		}
		if err := get("/repos/"+slug, &repo); err != nil {
			return "", nil, err
		}
		branch = repo.Default
	}
	var raw []struct {
		Sha    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
			Author  struct {
				Name string `json:"name"`
				Date string `json:"date"`
			} `json:"author"`
		} `json:"commit"`
	}
	if err := get(fmt.Sprintf("/repos/%s/commits?sha=%s&per_page=%d", slug, queryEscape(branch), limit), &raw); err != nil {
		return "", nil, err
	}
	out := make([]commitInfo, 0, len(raw))
	for _, c := range raw {
		subject, _, _ := strings.Cut(c.Commit.Message, "\n")
		out = append(out, commitInfo{Sha: c.Sha, Short: shortSha(c.Sha), Subject: strings.TrimSpace(subject), Date: c.Commit.Author.Date, Author: c.Commit.Author.Name})
	}
	return branch, out, nil
}

func queryEscape(s string) string {
	var b bytes.Buffer
	for _, c := range []byte(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~', c == '/':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// clearCache deletes a project's downloaded source (refused while a build of it runs).
func (a *Agent) clearCache(project string) error {
	a.mu.Lock()
	busy := a.current != "" && a.curProject == normProject(project)
	a.mu.Unlock()
	if busy {
		return errors.New("a build of this project is running; cancel it first")
	}
	mu := a.cloneLock(project)
	mu.Lock()
	defer mu.Unlock()
	a.sizeMu.Lock()
	delete(a.sizes, normProject(project))
	a.sizeMu.Unlock()
	return removeAll(a.cacheDir(project))
}

// removeAll deletes a folder tree, including the read-only files git keeps in .git/objects on Windows.
func removeAll(dir string) error {
	if err := os.RemoveAll(dir); err == nil {
		return nil
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil {
			_ = os.Chmod(p, 0o777)
		}
		return nil
	})
	return os.RemoveAll(dir)
}
