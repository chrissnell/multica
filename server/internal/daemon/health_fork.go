// This file holds fork-local daemon HTTP handlers that upstream does not have.
// Keeping them out of health.go confines the fork's divergence there to a
// single registration line in serveHealth (`/repo/refresh`), so routine
// upstream merges stop re-conflicting in the health server wiring. The
// controller-mode handlers below are registered by the fork's single-task
// runner (single_task.go), not by serveHealth.

package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/repocache"
)

// repoRefreshRequest is the body of a POST /repo/refresh request.
type repoRefreshRequest struct {
	URL         string `json:"url"`
	WorkspaceID string `json:"workspace_id"`
}

// controllerRepoCheckoutHandler is the controller-mode (Plan F.1) variant of
// repoCheckoutHandler. It's registered by SingleTaskRunner when the runner
// detects MULTICA_REPOCACHE_DIR — meaning the bare clones are externally
// managed by the multica-repocache Deployment and mounted ReadOnly into
// this worker pod.
//
// Differences from the daemon-mode handler:
//   - No ensureRepoReady: there's no per-daemon workspaceState, no refresh
//     loop, and no point checking workspaceRepoAllowed because the gitconfig
//     mounted into this pod already constrains which URLs can be rewritten
//     into the cache. The controller validates the workspace's repo list
//     when generating the gitconfig.
//   - Uses Cache.CreateSharedClone instead of CreateWorktree: a `git worktree
//     add` against a RO bare fails because git writes worktree metadata into
//     the bare. The shared-clone path uses alternates + a writable .git in
//     the workdir.
//   - Co-authored-by is resolved by fetching the workspace settings live
//     (fetchCoAuthoredByEnabled). Controller-mode workers have no synced view
//     of the setting, so the gate is read at checkout time; a fetch failure
//     resolves to off so a stale or unreachable settings view can never
//     re-enable attribution.
func (d *Daemon) controllerRepoCheckoutHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req repoCheckoutRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.URL == "" {
			http.Error(w, "url is required", http.StatusBadRequest)
			return
		}
		if req.WorkspaceID == "" {
			http.Error(w, "workspace_id is required", http.StatusBadRequest)
			return
		}
		if req.WorkDir == "" {
			http.Error(w, "workdir is required", http.StatusBadRequest)
			return
		}
		if d.repoCache == nil {
			http.Error(w, "repo cache not initialized", http.StatusInternalServerError)
			return
		}

		result, err := d.repoCache.CreateSharedClone(repocache.WorktreeParams{
			WorkspaceID:         req.WorkspaceID,
			RepoURL:             req.URL,
			WorkDir:             req.WorkDir,
			Ref:                 req.Ref,
			AgentName:           req.AgentName,
			TaskID:              req.TaskID,
			CoAuthoredByEnabled: d.fetchCoAuthoredByEnabled(r.Context(), req.WorkspaceID),
		})
		if err != nil {
			d.logger.Error("controller repo checkout failed", "url", req.URL, "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}
}

// repoRefreshHandler returns the POST /repo/refresh HTTP handler for daemon
// mode. It looks up the bare clone for (workspace_id, url) and runs
// `git fetch origin` on it. Used by `multica repo refresh` so an agent can
// force the cache to pick up commits that landed within the daemon's sync
// interval window. Returns 404 when the URL is not in the cache, 400 on
// missing fields, 500 on fetch failure.
func (d *Daemon) repoRefreshHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req repoRefreshRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.URL == "" {
			http.Error(w, "url is required", http.StatusBadRequest)
			return
		}
		if req.WorkspaceID == "" {
			http.Error(w, "workspace_id is required", http.StatusBadRequest)
			return
		}
		if d.repoCache == nil {
			http.Error(w, "repo cache not initialized", http.StatusInternalServerError)
			return
		}
		bare := d.repoCache.Lookup(req.WorkspaceID, req.URL)
		if bare == "" {
			http.Error(w, fmt.Sprintf("repo not found in cache: %s (workspace: %s)", req.URL, req.WorkspaceID), http.StatusNotFound)
			return
		}
		if err := d.repoCache.Fetch(bare); err != nil {
			d.logger.Error("repo refresh failed", "url", req.URL, "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "refreshed"})
	}
}

// controllerRepoRefreshHandler is the controller-mode variant of
// repoRefreshHandler. The bare clone is on a ReadOnly mount owned by the
// multica-repocache Deployment, so the local worker daemon cannot run a fetch
// against it. Instead, this handler proxies to the repocache server's admin
// endpoint at MULTICA_REPOCACHE_URL, which executes the fetch on the writable
// side of the same PVC.
//
// Returns 503 when MULTICA_REPOCACHE_URL is not set (controller-mode is
// indicated by MULTICA_REPOCACHE_DIR being present; if URL is missing too,
// the deployment is misconfigured and the agent's refresh request cannot
// land).
func (d *Daemon) controllerRepoRefreshHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req repoRefreshRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.URL == "" {
			http.Error(w, "url is required", http.StatusBadRequest)
			return
		}
		if req.WorkspaceID == "" {
			http.Error(w, "workspace_id is required", http.StatusBadRequest)
			return
		}
		adminBase := strings.TrimRight(os.Getenv("MULTICA_REPOCACHE_URL"), "/")
		if adminBase == "" {
			http.Error(w, "MULTICA_REPOCACHE_URL not set; controller is missing the repocache admin endpoint", http.StatusServiceUnavailable)
			return
		}
		// Build the admin /repos/fetch URL. The repocache admin server takes
		// workspace_id and url as query params (see cmd/multica-repocache/server.go).
		q := url.Values{}
		q.Set("workspace_id", req.WorkspaceID)
		q.Set("url", req.URL)
		fetchURL := adminBase + "/repos/fetch?" + q.Encode()
		client := &http.Client{Timeout: 5 * time.Minute}
		resp, err := client.Post(fetchURL, "application/x-www-form-urlencoded", nil)
		if err != nil {
			d.logger.Error("controller repo refresh: proxy failed", "url", req.URL, "error", err)
			http.Error(w, "proxy to repocache failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			// Mirror the upstream status so the agent sees 404 (not in cache)
			// vs 500 (fetch error) vs 400 (bad input) without translation.
			d.logger.Warn("controller repo refresh: upstream non-OK",
				"url", req.URL,
				"status", resp.StatusCode,
				"body", strings.TrimSpace(string(body)),
			)
			http.Error(w, strings.TrimSpace(string(body)), resp.StatusCode)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "refreshed"})
	}
}
