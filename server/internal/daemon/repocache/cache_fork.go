// This file holds fork-local repocache behaviour that upstream does not have.
// Keeping it in a separate file (rather than inline in cache.go) confines the
// fork's divergence to narrow call-site seams in the upstream code, so routine
// upstream merges stop re-conflicting in the shared cache functions. The
// upstream-owned entry points call into the symbols defined here:
//
//   - gitEnv()   -> appendForkGitConfig()      (SSH insteadOf rewrite)
//   - gitFetch() -> advanceLocalDefaultBranch() (worker-pod ref visibility)
//   - CreateSharedClone() and SlugFor() are additive; they have no upstream
//     counterpart and are called only from controller-mode (Plan F.1) code.

package repocache

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// appendForkGitConfig adds the fork's env-scoped git config to the environment
// returned by the upstream gitEnv(). It redirects https://github.com/ remotes
// through the SSH endpoint via url.insteadOf.
//
// The repocache pod mounts an SSH deploy key but has no HTTPS credential
// helper, so anonymous HTTPS clones of fresh repos fail with "could not read
// Username" when GitHub returns the credential challenge (rate-limited,
// private, or just a fresh anonymous clone of a public repo). Rewriting to SSH
// reuses the working auth path. Per-bare-repo origin URLs are stored in their
// own config and are not affected by this env-scoped rewrite for subsequent
// fetches — only the initial clone resolves the URL through the rewrite.
//
// It locates the GIT_CONFIG_COUNT that upstream's gitEnv already set, bumps it
// by one, and appends the new key/value pair at that next index so it never
// clobbers upstream's safe.directory entry or any inherited env-scoped config.
func appendForkGitConfig(env []string) []string {
	count := 0
	countIdx := -1
	for i, kv := range env {
		if v, ok := strings.CutPrefix(kv, "GIT_CONFIG_COUNT="); ok {
			if n, err := strconv.Atoi(v); err == nil {
				count = n
				countIdx = i
			}
		}
	}
	if countIdx < 0 {
		// No config block to extend; nothing to rewrite.
		return env
	}
	idx := strconv.Itoa(count)
	env[countIdx] = "GIT_CONFIG_COUNT=" + strconv.Itoa(count+1)
	return append(env,
		"GIT_CONFIG_KEY_"+idx+"=url.git@github.com:.insteadOf",
		"GIT_CONFIG_VALUE_"+idx+"=https://github.com/",
	)
}

// SlugFor returns the on-disk bare-directory name (e.g. "github.com+owner+repo.git")
// that Sync uses for the given repo URL. Useful for callers like the K8s
// controller that need to construct file:// URLs into a remote-mounted cache
// without doing a Sync first themselves.
func (c *Cache) SlugFor(_ string, url string) string {
	return bareDirName(url)
}

// advanceLocalDefaultBranch updates the bare repo's refs/heads/<default> to
// match refs/remotes/origin/<default>. Worker-pod worktrees fetch from the bare
// via git's default refspec (+refs/heads/*:refs/remotes/origin/*), so without
// this advance they observe the clone-time snapshot indefinitely even though
// the cache's refs/remotes/origin/* is fresh. Called by gitFetch after every
// successful fetch; non-fatal (see the call site in cache.go).
//
// Only the default branch is advanced: other refs/heads/* entries may be locked
// by worker-pod worktrees (created via `git worktree add`), and update-ref would
// silently desync those worktrees' HEAD/index. The default branch is
// conventionally never used as a worktree-locked branch by CreateWorktree /
// CreateSharedClone — they always create per-task branches off the default —
// but the worktree-lock check below is a defensive safeguard in case that
// convention is broken.
func advanceLocalDefaultBranch(barePath string) error {
	originRef := getRemoteDefaultBranch(barePath)
	if !strings.HasPrefix(originRef, "refs/remotes/origin/") {
		// No origin-tracked default (cache mid-migration, ambiguous, or
		// resolver fell through to a refs/heads/* legacy fallback). Nothing
		// to advance.
		return nil
	}
	branch := strings.TrimPrefix(originRef, "refs/remotes/origin/")
	localRef := "refs/heads/" + branch

	locked, err := worktreeLockedBranches(barePath)
	if err != nil {
		return fmt.Errorf("list worktrees: %w", err)
	}
	if locked[localRef] {
		return nil
	}

	cmd := exec.Command("git", "-C", barePath, "update-ref", localRef, originRef)
	cmd.Env = gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("update-ref %s: %s: %w", localRef, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// worktreeLockedBranches returns the set of refs/heads/* refs currently held
// by an active worktree on the bare repo. Branches in this set must not be
// updated via plain `update-ref`: git refuses to fetch into a worktree-locked
// branch (the protection that motivates the modern refspec), but update-ref
// has no such guard and would silently desync the worktree's HEAD/index.
//
// Parses `git worktree list --porcelain` blocks of the form:
//
//	worktree /path
//	HEAD <sha>
//	branch refs/heads/auto/claude-2.1.159
func worktreeLockedBranches(barePath string) (map[string]bool, error) {
	cmd := exec.Command("git", "-C", barePath, "worktree", "list", "--porcelain")

	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	locked := make(map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(line, "branch "); ok {
			locked[strings.TrimSpace(rest)] = true
		}
	}
	return locked, nil
}

// CreateSharedClone is the read-only-cache equivalent of CreateWorktree.
// Used by the controller-mode single-task runner where the bare clone is on
// a PVC mounted ReadOnly. `git worktree add` writes worktree metadata into
// the bare repo, which fails on a RO mount, so we use `git clone --shared`
// instead. The new clone shares object storage with the bare via alternates
// (no copy of pack data) but writes its own .git in the workdir, including
// the agent branch.
//
// After cloning, the remote origin URL is rewritten to the original repo URL
// (not the local cache path). That way the gitconfig CM the controller
// mounted at ~/.gitconfig — with `insteadOf` for fetch and `pushInsteadOf`
// for push — still routes fetches to the cache and pushes to SSH origin.
// If we left origin pointing at the cache path, pushes would target the RO
// PVC and fail because pushInsteadOf matches the original URL, not the
// rewritten one.
//
// Concurrency: the bare is RO so we don't take c.repoLocks. Multiple
// worker pods can read the same bare in parallel without any coordination.
func (c *Cache) CreateSharedClone(params WorktreeParams) (*WorktreeResult, error) {
	barePath := c.Lookup(params.WorkspaceID, params.RepoURL)
	if barePath == "" {
		return nil, fmt.Errorf("repo not found in cache: %s (workspace: %s) — controller-mode requires the repocache server to have synced this URL first", params.RepoURL, params.WorkspaceID)
	}

	// Skip gitFetch — the repocache server owns refresh in controller mode,
	// and the bare PVC is RO from the worker's perspective anyway.

	baseRef, err := resolveBaseRef(barePath, params.Ref)
	if err != nil {
		return nil, err
	}
	if baseRef == "" {
		return nil, fmt.Errorf("cannot resolve default branch for %s: bare cache at %s has no usable refs (origin/* is empty or ambiguous and bare HEAD has no match)", params.RepoURL, barePath)
	}

	branchName := fmt.Sprintf("agent/%s/%s", sanitizeName(params.AgentName), shortID(params.TaskID))
	dirName := repoNameFromURL(params.RepoURL)
	clonePath := filepath.Join(params.WorkDir, dirName)

	// Reused workdir from a prior task on the same issue. Refresh in place:
	// reset, clean, fetch (via gitconfig → cache), then create the new
	// agent branch from the resolved baseRef.
	if isGitWorktree(clonePath) || isGitRepo(clonePath) {
		actualBranch, err := refreshSharedClone(clonePath, branchName, baseRef)
		if err != nil {
			return nil, fmt.Errorf("refresh existing shared clone: %w", err)
		}
		applyHooksAndExcludes(clonePath, params.CoAuthoredByEnabled, c.logger)
		c.logger.Info("repo checkout: existing shared clone refreshed",
			"url", params.RepoURL,
			"path", clonePath,
			"branch", actualBranch,
			"base", baseRef,
		)
		return &WorktreeResult{Path: clonePath, BranchName: actualBranch}, nil
	}

	// Fresh clone from the bare at barePath on the mounted RO PVC. --shared
	// uses alternates so no object copy is needed — but git silently ignores
	// --local/--shared when the source is given as a URL (including file://),
	// so the source MUST be passed as a plain local path.
	if err := gitCloneShared(barePath, clonePath); err != nil {
		return nil, fmt.Errorf("git clone --shared: %w", err)
	}

	// Reset origin to the original URL so the gitconfig's insteadOf (for
	// fetch) and pushInsteadOf (for push) take effect on every future
	// operation against this clone. Without this, the stored URL would be
	// the local cache path and push would try to write to the RO PVC.
	if err := gitRemoteSetURL(clonePath, "origin", params.RepoURL); err != nil {
		return nil, fmt.Errorf("set origin url: %w", err)
	}

	// Create the agent branch from the resolved baseRef. Names live in the
	// new clone's refs/heads/, not the bare's, so there's no cross-task
	// branch-name collision possible.
	actualBranch, err := checkoutNewBranch(clonePath, branchName, baseRef)
	if err != nil {
		return nil, fmt.Errorf("checkout agent branch: %w", err)
	}

	applyHooksAndExcludes(clonePath, params.CoAuthoredByEnabled, c.logger)
	c.logger.Info("repo checkout: shared clone created",
		"url", params.RepoURL,
		"path", clonePath,
		"branch", actualBranch,
		"base", baseRef,
	)
	return &WorktreeResult{Path: clonePath, BranchName: actualBranch}, nil
}

// gitCloneShared runs `git clone --shared <barePath> <clonePath>`. The
// --shared flag stores an alternates pointer to barePath instead of copying
// pack data, which means the bare can stay on a read-only mount.
//
// barePath must be a plain local path, not a file:// URL: git silently
// ignores --local/--shared when the source is given as any URL form, and
// would fall back to a full object copy.
func gitCloneShared(barePath, clonePath string) error {
	cmd := exec.Command("git", "clone", "--shared", barePath, clonePath)
	cmd.Env = gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.RemoveAll(clonePath)
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// gitRemoteSetURL runs `git -C clonePath remote set-url <name> <url>`. Used
// to overwrite the clone's recorded origin URL after a shared clone from the
// local cache, so the gitconfig CM's insteadOf/pushInsteadOf rules apply on
// every subsequent fetch/push.
func gitRemoteSetURL(clonePath, name, url string) error {
	cmd := exec.Command("git", "-C", clonePath, "remote", "set-url", name, url)
	cmd.Env = gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// checkoutNewBranch creates and checks out a branch from baseRef inside an
// existing clone. On collision (a stale agent/* branch leaked from a prior
// run on this same workdir), it appends a timestamp and retries once —
// same behaviour the daemon-mode worktree-add path has.
func checkoutNewBranch(clonePath, branchName, baseRef string) (string, error) {
	cmd := exec.Command("git", "-C", clonePath, "checkout", "-b", branchName, baseRef)
	cmd.Env = gitEnv()
	if out, err := cmd.CombinedOutput(); err == nil {
		return branchName, nil
	} else {
		wrapped := fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
		if !isBranchCollisionError(wrapped) {
			return "", wrapped
		}
	}
	branchName = fmt.Sprintf("%s-%d", branchName, time.Now().Unix())
	cmd = exec.Command("git", "-C", clonePath, "checkout", "-b", branchName, baseRef)
	cmd.Env = gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git checkout -b (retry): %s: %w", strings.TrimSpace(string(out)), err)
	}
	return branchName, nil
}

// refreshSharedClone updates a reused per-issue clone for a new task:
// reset and clean any leftover changes, fetch latest refs (which go
// through the gitconfig rewrite to the cache), and create a fresh agent
// branch from baseRef. Mirrors updateExistingWorktree's contract.
func refreshSharedClone(clonePath, branchName, baseRef string) (string, error) {
	for _, args := range [][]string{
		{"reset", "--hard"},
		{"clean", "-fd"},
		{"fetch", "origin"},
	} {
		cmd := exec.Command("git", append([]string{"-C", clonePath}, args...)...)
		cmd.Env = gitEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(out)), err)
		}
	}
	return checkoutNewBranch(clonePath, branchName, baseRef)
}

// applyHooksAndExcludes installs/removes the co-authored-by hook and writes
// the agent-context exclude patterns. Best-effort: each helper logs its own
// warnings on failure, but nothing here is fatal to the checkout.
func applyHooksAndExcludes(clonePath string, coAuthoredByEnabled bool, logger *slog.Logger) {
	if coAuthoredByEnabled {
		if err := installCoAuthoredByHook(clonePath); err != nil {
			logger.Warn("repo checkout: install co-authored-by hook failed (non-fatal)", "error", err)
		}
	} else {
		if err := removeCoAuthoredByHook(clonePath); err != nil {
			logger.Warn("repo checkout: remove co-authored-by hook failed (non-fatal)", "error", err)
		}
	}
	for _, pattern := range agentGitExcludePatterns {
		_ = excludeFromGit(clonePath, pattern)
	}
}

// isGitRepo returns true if path/.git is a directory (a regular clone, as
// produced by `git clone --shared`). Distinguished from `isGitWorktree`,
// which detects the .git *file* that `git worktree add` writes.
func isGitRepo(path string) bool {
	info, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil && info.IsDir()
}
