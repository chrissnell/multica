package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// pvcCleanupGrace is how long an issue must have been terminal before its PVC
// is reclaimed. The live-Job gate already covers the running worker pod, but a
// follow-up task can be claimed and dispatched against a just-completed issue
// in the gap between this sweep listing Jobs and issuing the Delete; a short
// grace on the issue's updated_at keeps that window from racing a brand-new
// Job onto a deleted volume. Kept small so cleanup is still prompt.
const pvcCleanupGrace = 2 * time.Minute

// orphanPVCCleanupGrace is how long an issue-scoped PVC whose issue 404s
// (deleted, or invisible to this token) must have existed before it is
// reclaimed. Long on purpose: a 404 is ambiguous — a transient server blip
// returns it too — so we only act on PVCs old enough that a genuinely-deleted
// issue is far more likely than a transient miss. Mirrors the daemon's
// mtime-gated orphan path (gc.go orphanByMTime / GCOrphanTTL).
const orphanPVCCleanupGrace = 7 * 24 * time.Hour

// SweepDonePVCs reclaims per-issue workdir PVCs once their issue reaches a
// terminal state. EnsurePVC creates one PVC per (workspace, agent, issue) and
// nothing ever deleted them, so a busy workspace accumulates a PVC per issue
// forever. This sweep asks the server for each issue's status and deletes the
// PVC when the issue is done or cancelled.
//
// A PVC is only deleted when no controller-managed Job still references its
// issue: an agent commonly flips its own issue to done while its worker pod is
// still running, and deleting a mounted PVC just leaves it wedged in
// Terminating behind the kubelet's pvc-protection finalizer. Gating on the
// absence of a live Job keeps deletion to genuinely idle PVCs; the next sweep
// (~30s later) reclaims it once the Job is gone. A short pvcCleanupGrace on the
// issue's updated_at backstops the gap between listing Jobs and the Delete,
// where a follow-up task could otherwise dispatch a new Job onto the volume.
//
// Only issue-scoped PVCs carry a non-empty issue-id label — chat-session,
// autopilot, and per-task PVCs are skipped here by construction.
func SweepDonePVCs(ctx context.Context, cli *daemon.Client, k kubernetes.Interface, namespace string) error {
	pvcs, err := k.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelManagedBy + "=" + managedByValue,
	})
	if err != nil {
		return fmt.Errorf("list pvcs: %w", err)
	}

	busy, err := issuesWithLiveJobs(ctx, k, namespace)
	if err != nil {
		return err
	}

	for _, p := range pvcs.Items {
		issueID := p.Labels[labelIssueID]
		if issueID == "" || busy[issueID] {
			continue
		}
		status, err := cli.GetIssueGCCheck(ctx, issueID)
		if err != nil {
			// A definitive 404 means the issue is deleted or invisible to this
			// token. If the PVC has aged past the long orphan grace, the
			// deleted-issue reading is far likelier than a transient 404, so
			// reclaim it (no live Job references it — the busy check above
			// already excluded those). A 5xx / network error is transient:
			// leave the PVC for a later sweep.
			if daemon.IsNotFound(err) && time.Since(p.CreationTimestamp.Time) > orphanPVCCleanupGrace {
				_ = k.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, p.Name, metav1.DeleteOptions{})
			}
			continue
		}
		if status.Status != "done" && status.Status != "cancelled" {
			continue
		}
		if time.Since(status.UpdatedAt) < pvcCleanupGrace {
			continue
		}
		_ = k.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, p.Name, metav1.DeleteOptions{})
	}
	return nil
}

// issuesWithLiveJobs returns the set of issue ids that still have a
// controller-managed Job in the namespace, so their PVCs are left alone.
func issuesWithLiveJobs(ctx context.Context, k kubernetes.Interface, namespace string) (map[string]bool, error) {
	jobs, err := k.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelManagedBy + "=" + managedByValue,
	})
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	busy := make(map[string]bool, len(jobs.Items))
	for _, j := range jobs.Items {
		if id := j.Labels[labelIssueID]; id != "" {
			busy[id] = true
		}
	}
	return busy, nil
}

// taskPVCCleanupGrace is the minimum age a per-task workdir PVC must reach
// before it is reclaimed. A per-task PVC is created in the same dispatch pass
// as its Job, immediately before it; this grace keeps a sweep landing in that
// sub-second window from deleting a PVC whose Job is about to be created. Once
// the Job exists it pins the PVC for the task's lifetime plus the Job's
// TTLSecondsAfterFinished (1h) tail, so any unreferenced per-task PVC older
// than this is genuinely abandoned.
const taskPVCCleanupGrace = time.Hour

// SweepTaskPVCs reclaims per-task workdir PVCs whose worker Job is gone.
//
// Unlike an issue-scoped PVC (one per issue, reused by follow-up tasks on the
// same issue), a per-task PVC is created for a single task.ID and never reused,
// so once no live Job mounts it and it has aged past taskPVCCleanupGrace it is
// pure garbage. Correlation is by the PVC name the Job actually mounts, since
// task PVCs carry no issue-id label to join on.
//
// Chat-session (c…) and autopilot-run (a…) PVCs are intentionally left alone:
// they are reused across tasks and hold resumable session state with no
// terminal signal the controller can read from the PVC alone. Returns the
// number of PVCs deleted.
func SweepTaskPVCs(ctx context.Context, k kubernetes.Interface, namespace string) (int, error) {
	pvcs, err := k.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelManagedBy + "=" + managedByValue,
	})
	if err != nil {
		return 0, fmt.Errorf("list pvcs: %w", err)
	}

	referenced, err := pvcsReferencedByLiveJobs(ctx, k, namespace)
	if err != nil {
		return 0, err
	}

	deleted := 0
	for _, p := range pvcs.Items {
		if p.Labels[labelIssueID] != "" { // issue-scoped — SweepDonePVCs owns it
			continue
		}
		if !isPerTaskPVC(p.Name) { // chat / autopilot / malformed — leave alone
			continue
		}
		if referenced[p.Name] { // a live Job still mounts it
			continue
		}
		if time.Since(p.CreationTimestamp.Time) < taskPVCCleanupGrace {
			continue
		}
		if err := k.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, p.Name, metav1.DeleteOptions{}); err == nil {
			deleted++
		}
	}
	return deleted, nil
}

// pvcsReferencedByLiveJobs returns the set of PVC names mounted by a
// controller-managed Job's worker pod (the "work" volume). A PVC in this set
// has a live Job and must not be reclaimed.
func pvcsReferencedByLiveJobs(ctx context.Context, k kubernetes.Interface, namespace string) (map[string]bool, error) {
	jobs, err := k.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelManagedBy + "=" + managedByValue,
	})
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	refs := make(map[string]bool, len(jobs.Items))
	for _, j := range jobs.Items {
		for _, v := range j.Spec.Template.Spec.Volumes {
			if v.PersistentVolumeClaim != nil {
				refs[v.PersistentVolumeClaim.ClaimName] = true
			}
		}
	}
	return refs, nil
}

// isPerTaskPVC reports whether a managed PVC name is a per-task workdir, i.e.
// its scope segment is t<taskprefix>. pvcName() builds names as
// wd-<ws8>-<agent8>-<scope>; the 8-char UUID prefixes contain no dashes, so a
// well-formed name splits into exactly 4 parts. Callers gate on an empty
// issue-id label first, so the only ambiguous case (an issue's bare-hex scope
// that happens to start with other letters) never reaches here.
func isPerTaskPVC(name string) bool {
	parts := strings.Split(name, "-")
	if len(parts) != 4 {
		return false
	}
	return strings.HasPrefix(parts[3], "t")
}
