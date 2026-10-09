package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Projects the page always lists, even before a folder is set for them.
var knownProjects = []string{"ggc", "generalsx", "bobtista", "generalsonline"}

type ProjectInfo struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Repo       string   `json:"repo"`
	Mode       string   `json:"mode"` // auto (downloaded by the agent) or local (a folder you chose)
	Cached     bool     `json:"cached"`
	CacheDir   string   `json:"cache_dir"`
	CacheMB    int      `json:"cache_mb"`
	Head       string   `json:"head"`
	Dir        string   `json:"dir"`
	Exists     bool     `json:"exists"`
	HasPresets bool     `json:"has_presets"`
	Presets    []string `json:"presets"`
}

// projectDirs is every configured project folder (ggc included when set).
func (a *Agent) projectDirs() map[string]string {
	a.srcMu.Lock()
	defer a.srcMu.Unlock()
	out := map[string]string{}
	for id, d := range a.Srcs {
		if d != "" {
			out[id] = d
		}
	}
	if a.Src != "" {
		out["ggc"] = a.Src
	}
	return out
}

func (a *Agent) projectList() []ProjectInfo {
	dirs := a.projectDirs()
	ids := append([]string{}, knownProjects...)
	var extra []string
	for id := range dirs {
		known := false
		for _, k := range knownProjects {
			known = known || k == id
		}
		if !known {
			extra = append(extra, id)
		}
	}
	sort.Strings(extra)
	ids = append(ids, extra...)
	out := []ProjectInfo{}
	for _, id := range ids {
		p := ProjectInfo{ID: id, Name: id, Dir: dirs[id], Presets: []string{}, Mode: "local"}
		if k, ok := knownRepos[id]; ok {
			p.Name = k.Name
		}
		if u := a.repoURL(id); u != "" {
			p.Repo = strings.TrimSuffix(u, ".git")
		}
		if p.Dir == "" && p.Repo != "" {
			p.Mode = "auto"
			p.CacheDir = a.cacheDir(id)
			if a.cloned(id) {
				p.Cached, p.Exists, p.CacheMB, p.Head = true, true, a.cacheMB(id), a.cacheHead(id)
				if names, err := readPresetNames(p.CacheDir); err == nil {
					p.HasPresets, p.Presets = true, names
				}
			}
		}
		if p.Dir != "" {
			if st, err := os.Stat(p.Dir); err == nil && st.IsDir() {
				p.Exists = true
				if names, err := readPresetNames(p.Dir); err == nil {
					p.HasPresets, p.Presets = true, names
				}
			}
		}
		out = append(out, p)
	}
	return out
}

// handleProjects: GET lists the projects; POST {"id","dir"} sets (or with an empty dir clears) a project's folder.
func (a *Agent) handleProjects(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		if !a.projectsPost(w, r) {
			return
		}
	default:
		writeJSON(w, 405, map[string]string{"error": "GET or POST only"})
		return
	}
	writeJSON(w, 200, map[string]any{"projects": a.projectList()})
}

// projectsPost applies POST /api/projects; false means it already answered with an error.
func (a *Agent) projectsPost(w http.ResponseWriter, r *http.Request) bool {
	{
		var body struct {
			ID  string `json:"id"`
			Dir string `json:"dir"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad request body"})
			return false
		}
		if !reProject.MatchString(body.ID) {
			writeJSON(w, 400, map[string]string{"error": "bad project id"})
			return false
		}
		dir := strings.Trim(strings.TrimSpace(body.Dir), `"`)
		if dir != "" {
			if !filepath.IsAbs(dir) {
				writeJSON(w, 400, map[string]string{"error": "the folder must be an absolute path"})
				return false
			}
			dir = filepath.Clean(dir)
			if st, err := os.Stat(dir); err != nil || !st.IsDir() {
				writeJSON(w, 400, map[string]string{"error": dir + " is not a folder that exists on this computer"})
				return false
			}
			if _, err := os.Stat(filepath.Join(dir, "CMakePresets.json")); err != nil {
				writeJSON(w, 400, map[string]string{"error": "there is no CMakePresets.json in " + dir + ": is that the project's source checkout?"})
				return false
			}
		}
		a.setSrc(body.ID, dir)
		a.saveConfig()
	}
	return true
}

// handleClearCache: POST {"id"} deletes that project's downloaded source.
func (a *Agent) handleClearCache(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "POST only"})
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || !reProject.MatchString(body.ID) {
		writeJSON(w, 400, map[string]string{"error": "bad request body"})
		return
	}
	if err := a.clearCache(body.ID); err != nil {
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"projects": a.projectList()})
}

// agentConfig is what agent-config.json holds. Files written before projects existed have only Src and Data.
type agentConfig struct {
	Src, Data string
	Projects  map[string]string
}

func parseConfig(raw []byte) agentConfig {
	var c agentConfig
	_ = json.Unmarshal(raw, &c)
	return c
}
