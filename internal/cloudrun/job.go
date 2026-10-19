package cloudrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"

	run "google.golang.org/api/run/v1"
	"sigs.k8s.io/yaml"
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
	explicit: keepExplicitMaxRetries,
	retype:   quoteTaskTimeout,

	get: func(ctx context.Context, c *Client, name string) (*run.Job, error) {
		return fromAPI(c.api.Namespaces.Jobs.Get(c.jobName(name)).Context(ctx).Do())
	},
	create: func(ctx context.Context, c *Client, obj *run.Job, dryRun bool) (*run.Job, error) {
		call := c.api.Namespaces.Jobs.Create(c.parent(), obj)
		if dryRun {
			call = call.DryRun(dryRunAll)
		}
		return fromAPI(call.Context(ctx).Do())
	},
	replace: func(ctx context.Context, c *Client, name string, obj *run.Job, dryRun bool) (*run.Job, error) {
		call := c.api.Namespaces.Jobs.ReplaceJob(c.jobName(name), obj)
		if dryRun {
			call = call.DryRun(dryRunAll)
		}
		return fromAPI(call.Context(ctx).Do())
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
	task := taskSpec(j)
	if task == nil {
		return w
	}
	w.Containers = task.Containers
	w.Volumes = task.Volumes
	w.ServiceAccountName = task.ServiceAccountName
	return w
}

// taskSpec extracts a job's task spec (spec.template.spec.template.spec) nil-safely.
func taskSpec(j *run.Job) *run.TaskSpec {
	if j == nil || j.Spec == nil || j.Spec.Template == nil || j.Spec.Template.Spec == nil ||
		j.Spec.Template.Spec.Template == nil {
		return nil
	}
	return j.Spec.Template.Spec.Template.Spec
}

// maxRetries is the one field of a job where 0 is not the same as leaving it out. Confirmed
// against the real API: a task without maxRetries gets 3 (on create, and again on a replace that
// leaves it out), while an explicit 0 is kept and reads back as 0. The other integer fields probed —
// a job's parallelism, taskCount and task timeoutSeconds, a service's containerConcurrency,
// timeoutSeconds and probe periods — take 0 as unset, so sending or dropping their zeros changes
// nothing.
//
// The generated type drops an int64 that is 0 when it marshals, unless the field is named in
// ForceSendFields. Without that, `maxRetries: 0` in a manifest never reached the API, the job
// retried three times, and the diff — computed from the same marshalling — showed nothing.

// sendMaxRetries marks a job's maxRetries to be marshalled even when it is 0. Nothing else is
// changed, and a job without a task spec is left alone.
func sendMaxRetries(j *run.Job) {
	task := taskSpec(j)
	if task == nil || slices.Contains(task.ForceSendFields, "MaxRetries") {
		return
	}
	task.ForceSendFields = append(slices.Clip(task.ForceSendFields), "MaxRetries")
}

// fromAPI keeps maxRetries on a job the API returned. The API always sends the field (the default
// is filled in as 3), so a 0 here is one somebody set, and dropping it would turn it back into 3 on
// the next deploy of a manifest init wrote.
func fromAPI(j *run.Job, err error) (*run.Job, error) {
	if err == nil {
		sendMaxRetries(j)
	}
	return j, err
}

// keepExplicitMaxRetries keeps maxRetries on a parsed manifest that writes it, even as 0. A
// manifest that leaves it out still leaves it out, so the API's default applies.
func keepExplicitMaxRetries(manifest []byte, j *run.Job) error {
	var m map[string]any
	if err := yaml.Unmarshal(manifest, &m); err != nil {
		return fmt.Errorf("failed to parse the manifest: %w", err)
	}
	for _, key := range []string{"spec", "template", "spec", "template", "spec"} {
		next, ok := m[key].(map[string]any)
		if !ok {
			return nil
		}
		m = next
	}
	// A null (`maxRetries:` with nothing after it, as a template whose variable is unset renders)
	// leaves it out too: forcing it would send the 0 the null parsed into.
	if v, ok := m["maxRetries"]; ok && v != nil {
		sendMaxRetries(j)
	}
	return nil
}

// quoteTaskTimeout rewrites a numeric task timeoutSeconds into the string the generated type
// insists on. TaskSpec.TimeoutSeconds is tagged `json:",string"` (the API sends it as "600"), so
// `timeoutSeconds: 600` — how a service's timeout is written, and how anyone would write it —
// failed the strict parse with "invalid use of ,string struct tag", which does not say what to do.
// Only a whole number is rewritten; anything else is left for the original error.
func quoteTaskTimeout(manifest []byte) ([]byte, bool) {
	var m map[string]any
	if err := yaml.Unmarshal(manifest, &m); err != nil {
		return nil, false
	}
	task := m
	for _, key := range []string{"spec", "template", "spec", "template", "spec"} {
		next, ok := task[key].(map[string]any)
		if !ok {
			return nil, false
		}
		task = next
	}
	v, ok := task["timeoutSeconds"].(float64)
	if !ok || v != math.Trunc(v) {
		return nil, false
	}
	task["timeoutSeconds"] = strconv.FormatInt(int64(v), 10)
	out, err := json.Marshal(m)
	if err != nil {
		return nil, false
	}
	return out, true
}
