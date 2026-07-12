// This file holds the fork-local piece of the daemon workspace-repos response:
// folding project-scoped github_repo resources into the repo list the daemon
// receives. Keeping it out of daemon.go leaves workspaceReposResponse identical
// to upstream and confines the fork's divergence to a single addProjectRepos
// call at each response site.

package handler

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
)

// addProjectRepos folds every github_repo project resource attached to the
// workspace's projects into an already-built workspace-repos response, so
// attaching a github_repo to a project flows into the repocache automatically.
// A no-op when there are no project repos; recomputes ReposVersion after the
// merge so the daemon still detects the change.
func (h *Handler) addProjectRepos(ctx context.Context, workspaceID pgtype.UUID, resp *daemonWorkspaceReposResponse) {
	urls := h.lookupWorkspaceProjectRepoURLs(ctx, workspaceID)
	if len(urls) == 0 {
		return
	}
	repos := resp.Repos
	for _, url := range urls {
		repos = append(repos, RepoData{URL: url})
	}
	repos = normalizeWorkspaceRepos(repos)
	resp.Repos = repos
	resp.ReposVersion = workspaceReposVersion(repos)
}

// lookupWorkspaceProjectRepoURLs returns the URLs of every github_repo
// project resource attached to projects in the workspace. A query failure
// here is non-fatal — we log and return nil so the workspace-level repos are
// still served.
func (h *Handler) lookupWorkspaceProjectRepoURLs(ctx context.Context, workspaceID pgtype.UUID) []string {
	urls, err := h.Queries.ListWorkspaceGithubRepoURLs(ctx, workspaceID)
	if err != nil {
		slog.Warn("list workspace github_repo resources failed", "workspace_id", uuidToString(workspaceID), "error", err)
		return nil
	}
	return urls
}
