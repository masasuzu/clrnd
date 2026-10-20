package cloudrun

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	run "google.golang.org/api/run/v1"
)

// Annotations on a worker pool's metadata that set how many instances it runs. Worker pools do
// not scale on their own: the count is fixed, and changing it creates no revision.
const (
	annotationScalingMode         = "run.googleapis.com/scalingMode"
	annotationManualInstanceCount = "run.googleapis.com/manualInstanceCount"
	scalingModeManual             = "manual"
)

// workerPoolKind describes Cloud Run worker pools.
//
// Confirmed against the real API before it was put on the shared path: a write is a
// compare-and-swap on resourceVersion (409 when stale, 400 when malformed, an unconditional
// overwrite when absent), a dry run returns the definition with the server's defaults, and
// observedGeneration and Ready settle the way a service's do — also after a change to the
// instance count alone, which bumps the generation without creating a revision. Revision names
// behave as on a service: a generated one is not echoed back, and reusing a pinned one with a
// different configuration is a 409. Deletion is asynchronous.
var workerPoolKind = &kindOf[*run.WorkerPool]{
	name:       "WorkerPool",
	noun:       "worker pool",
	apiVersion: "run.googleapis.com/v1",

	newObject: func() *run.WorkerPool { return &run.WorkerPool{} },
	typeMeta:  func(w *run.WorkerPool) (string, string) { return w.ApiVersion, w.Kind },
	meta: func(w *run.WorkerPool) *run.ObjectMeta {
		if w == nil {
			return nil
		}
		return w.Metadata
	},
	workload: func(w *run.WorkerPool) workload {
		return templateWorkload("spec.template.spec", workerPoolTemplate(w))
	},
	status: newWorkerPoolWaitStatus,
	check:  checkWorkerPoolScaling,

	get: func(ctx context.Context, c *Client, name string) (*run.WorkerPool, error) {
		return c.api.Namespaces.Workerpools.Get(c.workerPoolName(name)).Context(ctx).Do()
	},
	create: func(ctx context.Context, c *Client, obj *run.WorkerPool, dryRun bool) (*run.WorkerPool, error) {
		call := c.api.Namespaces.Workerpools.Create(c.parent(), obj)
		if dryRun {
			call = call.DryRun(dryRunAll)
		}
		return call.Context(ctx).Do()
	},
	replace: func(ctx context.Context, c *Client, name string, obj *run.WorkerPool, dryRun bool) (*run.WorkerPool, error) {
		call := c.api.Namespaces.Workerpools.ReplaceWorkerPool(c.workerPoolName(name), obj)
		if dryRun {
			call = call.DryRun(dryRunAll)
		}
		return call.Context(ctx).Do()
	},
	del: func(ctx context.Context, c *Client, name string, dryRun bool) error {
		call := c.api.Namespaces.Workerpools.Delete(c.workerPoolName(name))
		if dryRun {
			call = call.DryRun(dryRunAll)
		}
		_, err := call.Context(ctx).Do()
		return err
	},

	prepare: func(desired, current *run.WorkerPool, name string, opts PlanOptions) (*run.WorkerPool, error) {
		if !opts.KeepSplit {
			return desired, nil
		}
		return keepInstanceSplit(desired, current, name)
	},
	template:  workerPoolTemplate,
	revisions: workerPoolRevisions,
	// A worker pool holds the same revision template as a service, so it gets the same handling
	// of revision names: init drops a pinned one and diff ignores a live one the manifest does not
	// pin.
	align:    alignRevisionNameOf(workerPoolTemplate, withWorkerPoolTemplate),
	scaffold: withoutRevisionNameOf(workerPoolTemplate, withWorkerPoolTemplate),
}

// workerPoolName builds the worker pool resource name for the namespaces API.
func (c *Client) workerPoolName(pool string) string {
	return fmt.Sprintf("namespaces/%s/workerpools/%s", c.project, pool)
}

// workerPoolTemplate returns a worker pool's spec.template nil-safely.
func workerPoolTemplate(w *run.WorkerPool) *run.RevisionTemplate {
	if w == nil || w.Spec == nil {
		return nil
	}
	return w.Spec.Template
}

// withWorkerPoolTemplate returns a copy of w holding t as its spec.template. It does not modify w:
// only the worker pool and its spec are copied.
func withWorkerPoolTemplate(w *run.WorkerPool, t *run.RevisionTemplate) *run.WorkerPool {
	out := *w
	spec := run.WorkerPoolSpec{}
	if w.Spec != nil {
		spec = *w.Spec
	}
	spec.Template = t
	out.Spec = &spec
	return &out
}

// checkWorkerPoolScaling rejects the instance-count annotations in the combinations that do not
// do what they appear to. Confirmed against the real API:
//
//   - Leaving both out keeps the count the worker pool runs now (1 on create). That is what lets
//     an external autoscaler, which works by rewriting manualInstanceCount, own the count while
//     clrnd deploys everything else.
//   - scalingMode: manual without a count sets the count to 0, which stops every instance. It
//     reads like "keep the manual count", so it is refused rather than sent.
//   - A count without scalingMode: manual is a 400 from the API; refusing it here says why before
//     anything needs credentials.
func checkWorkerPoolScaling(w *run.WorkerPool) []error {
	if w == nil || w.Metadata == nil {
		return nil
	}
	annotations := w.Metadata.Annotations
	mode, hasMode := annotations[annotationScalingMode]
	count, hasCount := annotations[annotationManualInstanceCount]
	var errs []error
	switch {
	case hasCount && strings.TrimSpace(mode) != scalingModeManual:
		errs = append(errs, fmt.Errorf("metadata.annotations: %s needs %s: %s alongside it",
			annotationManualInstanceCount, annotationScalingMode, scalingModeManual))
	case hasMode && strings.TrimSpace(mode) == scalingModeManual && !hasCount:
		errs = append(errs, fmt.Errorf("metadata.annotations: %s: %s without %s sets the worker pool to "+
			"0 instances, which stops it; write the count (0 to stop it on purpose), or leave both out "+
			"to keep the count it runs now", annotationScalingMode, scalingModeManual,
			annotationManualInstanceCount))
	}
	if hasCount {
		if n, err := strconv.Atoi(strings.TrimSpace(count)); err != nil || n < 0 {
			errs = append(errs, fmt.Errorf("metadata.annotations: %s must be a whole number of 0 or more, got %q",
				annotationManualInstanceCount, count))
		}
	}
	return errs
}
