package cloudrun

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	run "google.golang.org/api/run/v1"
)

func TestNewJobStatusReport(t *testing.T) {
	if got := newJobStatusReport(nil); got.Job != "" || got.LatestExecution != nil {
		t.Errorf("newJobStatusReport(nil) = %+v, want empty", got)
	}

	job := liveJob("gcr.io/project/job:v1")
	job.Status.ExecutionCount = 5
	job.Status.LatestCreatedExecution = &run.ExecutionReference{
		Name:                "my-job-abcde",
		CompletionStatus:    "EXECUTION_FAILED",
		CreationTimestamp:   "2026-01-02T03:04:05Z",
		CompletionTimestamp: "2026-01-02T03:05:00Z",
	}
	s := newJobStatusReport(job)
	if s.Job != "my-job" || s.Generation != 3 || s.ObservedGeneration != 3 || s.ExecutionCount != 5 {
		t.Errorf("newJobStatusReport() = %+v", s)
	}
	if e := s.LatestExecution; e == nil || e.Name != "my-job-abcde" || e.Status != "Failed" {
		t.Errorf("LatestExecution = %+v, want the failed execution", e)
	}
	// completionTime is the name jobs executions list uses; completed is the first name, kept with
	// the same value so jq filters written against it keep working.
	raw, err := json.Marshal(s.LatestExecution)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"completionTime":"2026-01-02T03:05:00Z"`, `"completed":"2026-01-02T03:05:00Z"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("LatestExecution JSON = %s, want it to contain %s", raw, want)
		}
	}

	// An unspecified status says nothing, so it is left out; one this code has not seen yet is
	// shown as the API wrote it rather than dropped.
	for raw, want := range map[string]string{"COMPLETION_STATUS_UNSPECIFIED": "", "EXECUTION_NEW_STATE": "EXECUTION_NEW_STATE"} {
		job.Status.LatestCreatedExecution.CompletionStatus = raw
		if got := newJobStatusReport(job).LatestExecution.Status; got != want {
			t.Errorf("completionStatus %s → %q, want %q", raw, got, want)
		}
	}
}

func TestJobStatusText(t *testing.T) {
	s := &JobStatus{
		Job:                "my-job",
		Generation:         3,
		ObservedGeneration: 3,
		ExecutionCount:     5,
		LatestExecution:    &ExecutionSummary{Name: "my-job-abcde", Status: "Succeeded", CompletionTime: "2026-01-02T03:05:00Z"},
		Conditions:         []Condition{{Type: "Ready", Status: "True"}},
	}
	want := "" +
		"Job:             my-job\n" +
		"Ready:           True\n" +
		"Generation:      3 (observed 3)\n" +
		"Executions:      5\n" +
		"Last execution:  my-job-abcde (Succeeded, completed 2026-01-02T03:05:00Z)\n" +
		"Conditions:\n" +
		"  Ready  True\n"
	if got := s.Text(); got != want {
		t.Errorf("Text() =\n%s\nwant\n%s", got, want)
	}

	// A job that has never run still says so, rather than leaving the line out.
	if got := (&JobStatus{Job: "my-job"}).Text(); !strings.Contains(got, "Executions:      0\n") ||
		strings.Contains(got, "Last execution") {
		t.Errorf("Text() for a job that never ran =\n%s", got)
	}
}

func TestLiveManifestForAJob(t *testing.T) {
	c, _ := newTestClient(t, func(*http.Request) (int, interface{}) {
		return http.StatusOK, liveJob("gcr.io/project/job:v1")
	})
	got, err := c.LiveManifest(context.Background(), KindJob, "my-job")
	if err != nil {
		t.Fatalf("LiveManifest() error = %v", err)
	}
	// What init writes has to be what deploy and diff accept, and nothing the server manages.
	if err := Validate(KindJob, got, "my-job"); err != nil {
		t.Errorf("the scaffolded manifest does not validate: %v\n%s", err, got)
	}
	for _, unwanted := range []string{"status:", "resourceVersion", "lastUpdatedTime", "creator", "uid"} {
		if strings.Contains(string(got), unwanted) {
			t.Errorf("manifest contains %q:\n%s", unwanted, got)
		}
	}
}

func TestDeleteAndWaitForAJob(t *testing.T) {
	gets := 0
	c, api := newTestClient(t, func(r *http.Request) (int, interface{}) {
		switch r.Method {
		case http.MethodDelete:
			return http.StatusOK, map[string]interface{}{}
		case http.MethodGet:
			gets++
			if gets < 3 { // still there for a while after the DELETE, as on the real API
				return http.StatusOK, liveJob("gcr.io/project/job:v1")
			}
		}
		return http.StatusNotFound, googleAPIError(404, "not found")
	})

	if err := c.Delete(context.Background(), KindJob, "my-job", true); err != nil {
		t.Fatalf("Delete(dry run) error = %v", err)
	}
	del := lastRequest(t, api, http.MethodDelete)
	if del.Path != myJobPath || !strings.Contains(del.Query, "dryRun=all") {
		t.Errorf("delete = %s?%s, want the job with dryRun=all", del.Path, del.Query)
	}

	var updates []string
	err := c.WaitDeleted(context.Background(), KindJob, "my-job", WaitOptions{
		Interval: time.Millisecond,
		OnUpdate: func(m string) { updates = append(updates, m) },
	})
	if err != nil || gets != 3 {
		t.Errorf("WaitDeleted() = %v after %d polls, want success at the first 404", err, gets)
	}
	if len(updates) != 1 || updates[0] != "still present" {
		t.Errorf("updates = %q", updates)
	}
}
