package cloudrun

import (
	"errors"
	"fmt"

	run "google.golang.org/api/run/v1"
)

// shiftInstanceSplit is ShiftTrafficTarget for a worker pool: it returns the live worker pool's
// definition with only spec.instanceSplits rewritten, the rest of the instances staying on the
// revision running the largest share. It does not modify its argument; it shallow-copies only the
// Spec. spec.template is not touched, so no revision is created.
func shiftInstanceSplit(live *run.WorkerPool, req SplitRequest) (*run.WorkerPool, error) {
	if live == nil || live.Spec == nil {
		return nil, errors.New("the live worker pool has no spec to update")
	}
	if err := validateSplitRequest(req, workerPoolSplitWords); err != nil {
		return nil, err
	}

	target := &run.InstanceSplit{Percent: req.Percent}
	self := req.Revision
	if req.Latest {
		target.LatestRevision = true
		// latestRevision resolves to the newest *created* revision (confirmed against the real
		// API): after deploy --no-promote, latestReadyRevisionName still names the revision
		// running the instances, while the new one is the latest. Excluding latestReady here would
		// refuse a canary of the new revision as "already running them all".
		if live.Status != nil {
			self = live.Status.LatestCreatedRevisionName
		}
	} else {
		target.RevisionName = req.Revision
	}

	splits := []*run.InstanceSplit{target}
	if req.Percent < 100 {
		if err := checkInstancesForSplit(live); err != nil {
			return nil, err
		}
		rest, err := remainderRevision(instancesByRevision(live), self, req.Percent, workerPoolSplitWords)
		if err != nil {
			return nil, err
		}
		splits = append(splits, &run.InstanceSplit{RevisionName: rest, Percent: 100 - req.Percent})
	}

	spec := *live.Spec
	spec.InstanceSplits = splits
	out := *live
	out.Spec = &spec
	return &out, nil
}

// checkInstancesForSplit refuses a split between two revisions when the worker pool runs a single
// instance. Every revision with a share needs an instance of its own (confirmed against the real
// API: "manual instances must be greater than the number of instance splits serving workload", a
// 400, for one instance split between two revisions; a count of 0, a stopped worker pool, accepts
// any split). Refusing it here says so before the prompt rather than after it.
func checkInstancesForSplit(live *run.WorkerPool) error {
	count := newWorkerPoolStatus(live).InstanceCount
	if count == nil || *count == 0 || *count >= 2 {
		return nil
	}
	return fmt.Errorf("the worker pool runs %d instance, and a split between two revisions needs one "+
		"for each; set %s to 2 or more first, or pass --percent 100",
		*count, annotationManualInstanceCount)
}

// keepInstanceSplit replaces the desired instance split with the one the live worker pool runs
// now, pinned by revision name, for deploy --no-promote. It does not modify its arguments; it
// returns a new definition with only the Spec shallow-copied.
//
// As for a service's traffic (keepTraffic), "do not give the new revision any instances" can only
// be written by naming the revisions that run them now: the new revision's name is not known until
// the apply, and latestRevision would hand it everything. Confirmed against the real API: the new
// revision becomes Ready at 0%, and the worker pool settles at Ready on the new generation.
func keepInstanceSplit(desired, current *run.WorkerPool, pool string) (*run.WorkerPool, error) {
	if current == nil {
		return nil, fmt.Errorf("cannot keep the instance split: worker pool %q does not exist yet, so "+
			"the first revision has to run the instances", pool)
	}
	pinned := pinnedInstanceSplits(current)
	if len(pinned) == 0 {
		return nil, fmt.Errorf("cannot keep the instance split: worker pool %q is not running any "+
			"revision yet", pool)
	}
	if desired.Spec == nil {
		return nil, errors.New("the manifest has no spec to update")
	}
	spec := *desired.Spec
	spec.InstanceSplits = pinned
	out := *desired
	out.Spec = &spec
	return &out, nil
}

// pinnedInstanceSplits returns the live worker pool's current split as a spec.instanceSplits
// pinned by revision name. Revisions at 0% are left out: they run nothing, and naming them would
// only keep them from being pruned.
func pinnedInstanceSplits(live *run.WorkerPool) []*run.InstanceSplit {
	var out []*run.InstanceSplit
	if live == nil || live.Status == nil {
		return out
	}
	for _, split := range live.Status.InstanceSplits {
		if split == nil || split.RevisionName == "" || split.Percent <= 0 {
			continue
		}
		out = append(out, &run.InstanceSplit{RevisionName: split.RevisionName, Percent: split.Percent})
	}
	return out
}
