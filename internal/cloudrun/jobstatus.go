package cloudrun

import (
	"context"
	"fmt"
	"strings"

	run "google.golang.org/api/run/v1"
)

// JobStatus is the current state of a job. It holds only read-only information taken from the API
// response, and it is also the structure of jobs status's JSON output.
type JobStatus struct {
	Job                string `json:"job"`
	Generation         int64  `json:"generation,omitempty"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	// ExecutionCount is how many executions the job has had.
	ExecutionCount  int64             `json:"executionCount"`
	LatestExecution *ExecutionSummary `json:"latestExecution,omitempty"`
	Conditions      []Condition       `json:"conditions,omitempty"`
}

// ExecutionSummary is what a job records about its most recent execution, without fetching the
// execution itself.
type ExecutionSummary struct {
	Name string `json:"name"`
	// Status is how it ended, or that it has not: Succeeded, Failed, Cancelled, Running or
	// Pending. Empty when the API does not say.
	Status  string `json:"status,omitempty"`
	Created string `json:"created,omitempty"`
	// CompletionTime is when it finished; empty while it has not. It has the same name as an
	// Execution's (jobs executions list), so a jq filter written against one works on the other.
	CompletionTime string `json:"completionTime,omitempty"`
	// Completed holds the same value as CompletionTime. It was this field's first name and is kept
	// for JSON backward compatibility (so users who wrote jq '.latestExecution.completed' are not
	// silently broken). New code should read CompletionTime.
	Completed string `json:"completed,omitempty"`
}

// completionStatuses maps the API's ExecutionReference.completionStatus onto the words status
// prints. An unknown value is shown as the API wrote it rather than dropped.
var completionStatuses = map[string]string{
	"EXECUTION_SUCCEEDED": "Succeeded",
	"EXECUTION_FAILED":    "Failed",
	"EXECUTION_CANCELLED": "Cancelled",
	"EXECUTION_RUNNING":   "Running",
	"EXECUTION_PENDING":   "Pending",
}

// GetJobStatus fetches the current state of the job. It only reads and changes nothing.
func (c *Client) GetJobStatus(ctx context.Context, job string) (*JobStatus, error) {
	obj, err := jobKind.get(ctx, c, job)
	if err != nil {
		return nil, fmt.Errorf("failed to get job %q: %w", job, err)
	}
	return newJobStatusReport(obj), nil
}

// newJobStatusReport converts an API response into a JobStatus. It is pure, so the formatting is
// testable without the API.
func newJobStatusReport(obj *run.Job) *JobStatus {
	s := &JobStatus{}
	if obj == nil {
		return s
	}
	if obj.Metadata != nil {
		s.Job = obj.Metadata.Name
		s.Generation = obj.Metadata.Generation
	}
	if obj.Status == nil {
		return s
	}
	s.ObservedGeneration = obj.Status.ObservedGeneration
	s.ExecutionCount = obj.Status.ExecutionCount
	s.Conditions = newConditions(obj.Status.Conditions)
	if e := obj.Status.LatestCreatedExecution; e != nil && e.Name != "" {
		status, ok := completionStatuses[e.CompletionStatus]
		if !ok && e.CompletionStatus != "COMPLETION_STATUS_UNSPECIFIED" {
			status = e.CompletionStatus
		}
		s.LatestExecution = &ExecutionSummary{
			Name:           e.Name,
			Status:         status,
			Created:        e.CreationTimestamp,
			CompletionTime: e.CompletionTimestamp,
			Completed:      e.CompletionTimestamp,
		}
	}
	return s
}

// Ready returns the Ready condition (the job can be executed), or nil if there is none.
func (s *JobStatus) Ready() *Condition {
	for i := range s.Conditions {
		if s.Conditions[i].Type == conditionReady {
			return &s.Conditions[i]
		}
	}
	return nil
}

// Text returns the human-readable formatted output. It ends with a newline.
func (s *JobStatus) Text() string {
	var b strings.Builder
	line := func(label, value string) {
		if value == "" {
			return
		}
		fmt.Fprintf(&b, "%-*s%s\n", statusLabelWidth, label+":", value)
	}

	line("Job", s.Job)
	writeReady(line, s.Ready())
	if s.Generation != 0 || s.ObservedGeneration != 0 {
		line("Generation", fmt.Sprintf("%d (observed %d)", s.Generation, s.ObservedGeneration))
	}
	line("Executions", fmt.Sprintf("%d", s.ExecutionCount))
	if e := s.LatestExecution; e != nil {
		detail := e.Name
		var parts []string
		if e.Status != "" {
			parts = append(parts, e.Status)
		}
		if e.CompletionTime != "" {
			parts = append(parts, "completed "+e.CompletionTime)
		} else if e.Created != "" {
			parts = append(parts, "created "+e.Created)
		}
		if len(parts) > 0 {
			detail += " (" + strings.Join(parts, ", ") + ")"
		}
		line("Last execution", detail)
	}
	writeConditions(&b, s.Conditions)
	return b.String()
}
