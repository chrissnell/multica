package main

import (
	"context"
	"fmt"
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
			// Transient lookup failure (or a deleted/inaccessible issue, which
			// returns an ambiguous 404). Leave the PVC for a later sweep rather
			// than risk deleting one whose issue is merely unreachable.
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
