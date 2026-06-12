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
			Labels: map[string]string{
				labelManagedBy: managedByValue,
				labelIssueID:   issueID,
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
