package cloudrun

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	run "google.golang.org/api/run/v1"
)

// Executions is a job's executions, newest first. It is also the structure of the JSON output.
type Executions []*Execution

// ListExecutions returns the executions of a job, newest first. It only reads.
//
// The job is fetched first, so a job that does not exist (a typo, the wrong project) is an error
// rather than an empty list, which would read as "it never ran". A list that does not page through
// to the end is an error too (see listAll).
func (c *Client) ListExecutions(ctx context.Context, job string) (Executions, error) {
	if _, err := jobKind.get(ctx, c, job); err != nil {
		return nil, fmt.Errorf("failed to get job %q: %w", job, err)
	}
	selector := fmt.Sprintf("%s=%s", jobLabel, job)
	items, err := listAll(fmt.Sprintf("executions of job %q", job),
		func(token string) ([]*run.Execution, string, error) {
			call := c.api.Namespaces.Executions.List(c.parent()).LabelSelector(selector).Limit(listPageLimit)
			if token != "" {
				call = call.Continue(token)
			}
			resp, err := call.Context(ctx).Do()
			if err != nil {
				return nil, "", err
			}
			return resp.Items, continueToken(resp.Metadata), nil
		})
	if err != nil {
		return nil, err
	}
	return newExecutions(items), nil
}

// newExecutions converts the API's executions and sorts them newest first. The API already
// returns them in that order, but nothing promises it across pages, so the order is not left to
// it. An execution whose creation time does not parse goes last, by name.
func newExecutions(items []*run.Execution) Executions {
	out := make(Executions, 0, len(items))
	for _, item := range items {
		out = append(out, newExecution(item))
	}
	created := func(e *Execution) (time.Time, bool) {
		t, err := time.Parse(time.RFC3339Nano, e.Created)
		return t, err == nil
	}
	sort.SliceStable(out, func(i, j int) bool {
		ti, oki := created(out[i])
		tj, okj := created(out[j])
		switch {
		case oki && okj && !ti.Equal(tj):
			return ti.After(tj)
		case oki != okj:
			return oki
		}
		return out[i].Name > out[j].Name
	})
	return out
}

// Text formats the executions as a table. Empty when there are none.
func (es Executions) Text() string {
	if len(es) == 0 {
		return ""
	}
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "EXECUTION\tSTATUS\tTASKS\tCREATED\tCOMPLETED")
	for _, e := range es {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			dash(e.Name), dash(e.Status), e.TaskSummary(), dash(e.Created), dash(e.CompletionTime))
	}
	// tabwriter only writes on Flush. Writing to a builder cannot fail.
	_ = w.Flush()
	return b.String()
}

// TaskSummary is what the TASKS column shows: how many tasks succeeded out of how many, then whichever of the
// other counts is not zero, e.g. "0/3 succeeded, 1 running, 1 failed".
func (e *Execution) TaskSummary() string {
	s := fmt.Sprintf("%d succeeded", e.Succeeded)
	if e.TaskCount > 0 {
		s = fmt.Sprintf("%d/%d succeeded", e.Succeeded, e.TaskCount)
	}
	for _, n := range []struct {
		count int64
		label string
	}{{e.Running, "running"}, {e.Failed, "failed"}, {e.Cancelled, "cancelled"}} {
		if n.count > 0 {
			s += fmt.Sprintf(", %d %s", n.count, n.label)
		}
	}
	return s
}

// CancelExecution asks Cloud Run to cancel a running execution. It returns as soon as the request
// is accepted; the execution stops shortly after (within half a minute when tried against the real
// API), which WaitExecutionStopped waits for. Cloud Run has no dry run for a cancel, and refuses one
// for an execution that is not running with a 400.
func (c *Client) CancelExecution(ctx context.Context, execution string) error {
	_, err := c.api.Namespaces.Executions.Cancel(c.executionName(execution), &run.CancelExecutionRequest{}).
		Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("failed to cancel execution %q: %w", execution, err)
	}
	return nil
}
