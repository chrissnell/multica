package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/multica-ai/multica/server/internal/daemon"
)

// gcCheckServer returns a daemon client whose issue gc-check responds with the
// status mapped per issue id. updated_at is reported an hour in the past so the
// cleanup grace period is satisfied; use gcCheckServerAt to control the age.
// Unknown ids 404 like a deleted/inaccessible issue.
func gcCheckServer(t *testing.T, statuses map[string]string) *daemon.Client {
	return gcCheckServerAt(t, statuses, -time.Hour)
}

// gcCheckServerAt is gcCheckServer with an explicit updated_at offset from now,
// letting a test place a terminal issue inside or outside the cleanup grace.
func gcCheckServerAt(t *testing.T, statuses map[string]string, age time.Duration) *daemon.Client {
	t.Helper()
	updatedAt := time.Now().Add(age).UTC().Format(time.RFC3339)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for id, st := range statuses {
			if strings.Contains(r.URL.Path, id) {
				_, _ = io.WriteString(w, fmt.Sprintf(`{"status":%q,"updated_at":%q}`, st, updatedAt))
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"issue not found"}`)
	}))
	t.Cleanup(srv.Close)
	cli := daemon.NewClient(srv.URL)
	cli.SetToken("tk")
	return cli
}

func issuePVC(name, issueID string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "multica",
			CreationTimestamp: metav1.Now(),
			Labels: map[string]string{
				labelManagedBy: managedByValue,
				labelIssueID:   issueID,
			},
		},
	}
}

// issuePVCAged is issuePVC with an explicit age, for the long-orphan 404 path.
func issuePVCAged(name, issueID string, age time.Duration) *corev1.PersistentVolumeClaim {
	p := issuePVC(name, issueID)
	p.CreationTimestamp = metav1.NewTime(time.Now().Add(-age))
	return p
}

// taskPVC builds an unscoped (no issue-id) per-task workdir PVC named like
// pvcName()'s t<task8> form, created `age` ago. Mirrors EnsurePVC, which sets
// an empty issue-id label on non-issue PVCs.
func taskPVC(name string, age time.Duration) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "multica",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
			Labels: map[string]string{
				labelManagedBy: managedByValue,
				labelIssueID:   "",
			},
		},
	}
}

// jobMountingPVC builds a controller-managed Job whose worker pod mounts the
// named PVC via the "work" volume, exactly as DispatchJob does.
func jobMountingPVC(jobName, claimName string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: jobName, Namespace: "multica",
			Labels: map[string]string{labelManagedBy: managedByValue},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Volumes: []corev1.Volume{{
						Name: "work",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claimName},
						},
					}},
				},
			},
		},
	}
}

func pvcExists(t *testing.T, k *fake.Clientset, name string) bool {
	t.Helper()
	_, err := k.CoreV1().PersistentVolumeClaims("multica").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

func TestSweepDonePVCs_DeletesTerminalIssuePVCs(t *testing.T) {
	cli := gcCheckServer(t, map[string]string{
		"issDONE":   "done",
		"issCANCEL": "cancelled",
		"issLIVE":   "in_progress",
	})
	k := fake.NewSimpleClientset(
		issuePVC("wd-done", "issDONE"),
		issuePVC("wd-cancel", "issCANCEL"),
		issuePVC("wd-live", "issLIVE"),
	)

	if err := SweepDonePVCs(context.Background(), cli, k, "multica"); err != nil {
		t.Fatal(err)
	}

	if pvcExists(t, k, "wd-done") {
		t.Error("expected done-issue PVC to be deleted")
	}
	if pvcExists(t, k, "wd-cancel") {
		t.Error("expected cancelled-issue PVC to be deleted")
	}
	if !pvcExists(t, k, "wd-live") {
		t.Error("expected in_progress-issue PVC to be retained")
	}
}

func TestSweepDonePVCs_KeepsRecentlyTerminalPVC(t *testing.T) {
	// Done, but only just now — inside the cleanup grace, so a follow-up task
	// that dispatches in this window can't have its volume pulled out.
	cli := gcCheckServerAt(t, map[string]string{"issDONE": "done"}, -10*time.Second)
	k := fake.NewSimpleClientset(issuePVC("wd-done", "issDONE"))

	if err := SweepDonePVCs(context.Background(), cli, k, "multica"); err != nil {
		t.Fatal(err)
	}

	if !pvcExists(t, k, "wd-done") {
		t.Error("expected recently-terminal PVC to be retained within the grace period")
	}
}

func TestSweepDonePVCs_KeepsPVCWithLiveJob(t *testing.T) {
	cli := gcCheckServer(t, map[string]string{"issDONE": "done"})
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "task-running", Namespace: "multica",
			Labels: map[string]string{
				labelManagedBy: managedByValue,
				labelIssueID:   "issDONE",
			},
		},
	}
	k := fake.NewSimpleClientset(issuePVC("wd-done", "issDONE"), job)

	if err := SweepDonePVCs(context.Background(), cli, k, "multica"); err != nil {
		t.Fatal(err)
	}

	if !pvcExists(t, k, "wd-done") {
		t.Error("expected PVC with a live Job to be retained")
	}
}

func TestSweepDonePVCs_SkipsUnscopedAndInaccessible(t *testing.T) {
	cli := gcCheckServer(t, nil) // every gc-check 404s
	k := fake.NewSimpleClientset(
		issuePVC("wd-deleted", "issGONE"), // gc-check 404 → leave it
		issuePVC("wd-chat", ""),           // no issue label → not our concern
	)

	if err := SweepDonePVCs(context.Background(), cli, k, "multica"); err != nil {
		t.Fatal(err)
	}

	if !pvcExists(t, k, "wd-deleted") {
		t.Error("expected inaccessible-issue PVC to be retained")
	}
	if !pvcExists(t, k, "wd-chat") {
		t.Error("expected non-issue PVC to be retained")
	}
}

func TestSweepDonePVCs_ReclaimsOldOrphan404PVC(t *testing.T) {
	cli := gcCheckServer(t, nil) // every gc-check 404s (deleted/inaccessible issue)
	k := fake.NewSimpleClientset(issuePVCAged("wd-gone", "issGONE", 8*24*time.Hour))

	if err := SweepDonePVCs(context.Background(), cli, k, "multica"); err != nil {
		t.Fatal(err)
	}
	if pvcExists(t, k, "wd-gone") {
		t.Error("expected long-orphaned 404 issue PVC to be reclaimed")
	}
}

func TestSweepDonePVCs_KeepsRecentOrphan404PVC(t *testing.T) {
	cli := gcCheckServer(t, nil) // 404s, but the PVC is young
	k := fake.NewSimpleClientset(issuePVCAged("wd-gone", "issGONE", time.Hour))

	if err := SweepDonePVCs(context.Background(), cli, k, "multica"); err != nil {
		t.Fatal(err)
	}
	if !pvcExists(t, k, "wd-gone") {
		t.Error("expected a recently-created 404 PVC to be retained (transient-404 guard)")
	}
}

func TestSweepTaskPVCs_DeletesOrphanedPerTaskPVC(t *testing.T) {
	k := fake.NewSimpleClientset(taskPVC("wd-ws00-ag00-t12345678", 2*time.Hour))

	deleted, err := SweepTaskPVCs(context.Background(), k, "multica")
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Errorf("expected 1 deletion, got %d", deleted)
	}
	if pvcExists(t, k, "wd-ws00-ag00-t12345678") {
		t.Error("expected orphaned per-task PVC to be deleted")
	}
}

func TestSweepTaskPVCs_KeepsPVCWithLiveJob(t *testing.T) {
	k := fake.NewSimpleClientset(
		taskPVC("wd-ws00-ag00-t12345678", 2*time.Hour),
		jobMountingPVC("task-running", "wd-ws00-ag00-t12345678"),
	)

	deleted, err := SweepTaskPVCs(context.Background(), k, "multica")
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Errorf("expected 0 deletions, got %d", deleted)
	}
	if !pvcExists(t, k, "wd-ws00-ag00-t12345678") {
		t.Error("expected per-task PVC with a live Job to be retained")
	}
}

func TestSweepTaskPVCs_KeepsRecentPVC(t *testing.T) {
	// Younger than taskPVCCleanupGrace: covers the EnsurePVC→DispatchJob window
	// where the Job for this PVC may not exist yet.
	k := fake.NewSimpleClientset(taskPVC("wd-ws00-ag00-t12345678", time.Minute))

	deleted, err := SweepTaskPVCs(context.Background(), k, "multica")
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Errorf("expected 0 deletions, got %d", deleted)
	}
	if !pvcExists(t, k, "wd-ws00-ag00-t12345678") {
		t.Error("expected recently-created per-task PVC to be retained")
	}
}

func TestSweepTaskPVCs_IgnoresChatAutopilotAndIssuePVCs(t *testing.T) {
	k := fake.NewSimpleClientset(
		taskPVC("wd-ws00-ag00-c12345678", 2*time.Hour), // chat session — reused, leave it
		taskPVC("wd-ws00-ag00-a12345678", 2*time.Hour), // autopilot run — reused, leave it
		issuePVC("wd-ws00-ag00-12345678", "iss12345"),  // issue-scoped — not this sweep's job
	)

	deleted, err := SweepTaskPVCs(context.Background(), k, "multica")
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Errorf("expected 0 deletions, got %d", deleted)
	}
	if !pvcExists(t, k, "wd-ws00-ag00-c12345678") {
		t.Error("expected chat PVC to be retained")
	}
	if !pvcExists(t, k, "wd-ws00-ag00-a12345678") {
		t.Error("expected autopilot PVC to be retained")
	}
	if !pvcExists(t, k, "wd-ws00-ag00-12345678") {
		t.Error("expected issue PVC to be retained by the task sweep")
	}
}
