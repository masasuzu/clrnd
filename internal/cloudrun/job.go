package cloudrun

import (
	"context"
	"errors"
	"fmt"

	run "google.golang.org/api/run/v1"
)

// jobKind describes Cloud Run jobs.
//
// Confirmed against the real API: a job's write is a compare-and-swap on resourceVersion exactly
// like a service's (409 when stale, 400 when malformed, an unconditional overwrite when absent), a
// dry run returns the job with the server's defaults filled in, and after a replace
// observedGeneration catches up within seconds while Ready stays True — so the shared plan, diff
// and wait apply unchanged.
var jobKind = &kindOf[*run.Job]{
	name:       "Job",
	noun:       "job",
	apiVersion: "run.googleapis.com/v1",

	// Starting an execution updates the job's status, and with it its resourceVersion (once when
	// the execution starts and again when it finishes). A deploy whose diff was computed before a
	// run and applied after it gets the 409 even though nobody edited the job.
	conflictHint: " (starting an execution changes it too)",

	newObject: func() *run.Job { return &run.Job{} },
	typeMeta:  func(j *run.Job) (string, string) { return j.ApiVersion, j.Kind },
	meta: func(j *run.Job) *run.ObjectMeta {
		if j == nil {
			return nil
		}
		return j.Metadata
	},
	workload: jobWorkload,
	status:   newJobStatus,

	get: func(ctx context.Context, c *Client, name string) (*run.Job, error) {
		return c.api.Namespaces.Jobs.Get(c.jobName(name)).Context(ctx).Do()
	},
	create: func(ctx context.Context, c *Client, obj *run.Job, dryRun bool) (*run.Job, error) {
		call := c.api.Namespaces.Jobs.Create(c.parent(), obj)
		if dryRun {
			call = call.DryRun(dryRunAll)
		}
		return call.Context(ctx).Do()
	},
	replace: func(ctx context.Context, c *Client, name string, obj *run.Job, dryRun bool) (*run.Job, error) {
		call := c.api.Namespaces.Jobs.ReplaceJob(c.jobName(name), obj)
		if dryRun {
			call = call.DryRun(dryRunAll)
		}
		return call.Context(ctx).Do()
	},
	del: func(ctx context.Context, c *Client, name string, dryRun bool) error {
		call := c.api.Namespaces.Jobs.Delete(c.jobName(name))
		if dryRun {
			call = call.DryRun(dryRunAll)
		}
		_, err := call.Context(ctx).Do()
		return err
	},

	prepare: func(desired, _ *run.Job, _ string, opts PlanOptions) (*run.Job, error) {
		if opts.KeepTraffic {
			return nil, errors.New("a job serves no traffic, so there is no traffic to keep")
		}
		return desired, nil
	},
}

// jobName builds the job resource name for the namespaces API.
func (c *Client) jobName(job string) string {
	return fmt.Sprintf("namespaces/%s/jobs/%s", c.project, job)
}

// jobWorkload extracts a job's workload nil-safely. A job keeps its containers one template
// deeper than a service — spec.template (the execution) → spec.template (the task) → spec — while
// the annotations Cloud Run reads (VPC connector, Cloud SQL, secret aliases) are on the execution
// template's metadata.
func jobWorkload(j *run.Job) workload {
	w := workload{Path: "spec.template.spec.template.spec"}
	if j == nil || j.Spec == nil || j.Spec.Template == nil {
		return w
	}
	if meta := j.Spec.Template.Metadata; meta != nil {
		w.Annotations = meta.Annotations
	}
	exec := j.Spec.Template.Spec
	if exec == nil || exec.Template == nil || exec.Template.Spec == nil {
		return w
	}
	task := exec.Template.Spec
	w.Containers = task.Containers
	w.Volumes = task.Volumes
	w.ServiceAccountName = task.ServiceAccountName
	return w
}
