package cloudrun

import (
	"errors"
	"fmt"

	run "google.golang.org/api/run/v1"
)

// SelectRollbackRevision decides which revision to roll back to. It assumes revisions is in the
// "newest first" order ListRevisions returns.
//
// If requested is given, it returns that revision (after confirming it belongs to this service).
// Otherwise it picks the first Ready revision older than the one currently receiving traffic.
// An old revision that has lost its traffic stays Ready=True (Reason=Retired), so this finds
// "the version that was running just before".
//
// "The revision currently receiving traffic" means the *newest* one, not the one with the largest
// share. Mid-canary (new 10% / stable 90%), choosing by share mistakes the stable version for the
// current one and rolls back to the one before it, skipping past the known-good version.
//
// Without requested, it refuses when the newest revision serves nothing and is not Ready: the
// state a failed deploy leaves (or one still rolling out). Traffic that follows the latest
// revision then stays on the last one that became ready, so the revision serving now is the good
// one, and stepping back from it would take production further back than the deploy that failed.
// Pinning the serving revision instead would not help either: the template still describes the
// failed revision, so the service stays not Ready and the rollback's wait would fail anyway.
func SelectRollbackRevision(revisions Revisions, requested string) (*Revision, error) {
	return selectRollbackRevision(revisions, requested, serviceSplitWords)
}

// splitWords are how messages about what a kind splits between its revisions put it: a service's
// traffic, a worker pool's instances.
type splitWords struct {
	noun    string // the resource ("service")
	serving string // a revision with a share ("receiving traffic")
	idle    string // a revision without one ("serves no traffic")
	holder  string // the split, as the subject of "is still on" ("traffic is")
	wait    string // the command that waits for the rollout
	sendTo  string // giving a revision a share, as in "no revision to send traffic to"
	verb    string // a revision having a share, before what it has ("serving")
	what    string // what is split ("traffic")
}

var (
	serviceSplitWords = splitWords{
		noun: "service", serving: "receiving traffic", idle: "serves no traffic",
		holder: "traffic is", wait: "clrnd wait",
		sendTo: "send traffic to", verb: "serving", what: "traffic",
	}
	workerPoolSplitWords = splitWords{
		noun: "worker pool", serving: "running instances", idle: "runs no instances",
		holder: "the instances are", wait: "clrnd worker-pools wait",
		sendTo: "run the instances on", verb: "running", what: "instances",
	}
)

func selectRollbackRevision(revisions Revisions, requested string, words splitWords) (*Revision, error) {
	if requested != "" {
		return findRevision(revisions, requested, words.noun)
	}

	// revisions is newest first, so the first one found "with a share" is the newest serving
	// version.
	current := -1
	for i, r := range revisions {
		if r.Percent > 0 {
			current = i
			break
		}
	}
	if current < 0 {
		return nil, fmt.Errorf("no revision is currently %s; pass --revision to choose one", words.serving)
	}
	if newest := revisions[0]; current > 0 && !newest.IsReady() {
		return nil, unfinishedRolloutError(newest, revisions[current], words)
	}

	for i := current + 1; i < len(revisions); i++ {
		if revisions[i].Ready == conditionTrue {
			return &revisions[i], nil
		}
	}
	return nil, fmt.Errorf("no ready revision older than %q to roll back to; pass --revision to choose one",
		revisions[current].Name)
}

// findRevision returns the revision in the list whose name matches. It is an error if none does.
// It is also the check that "this is a revision of this resource": sending a share to a mistyped
// name would produce a split that reaches no revision at all.
func findRevision(revisions Revisions, name, noun string) (*Revision, error) {
	for i := range revisions {
		if revisions[i].Name == name {
			return &revisions[i], nil
		}
	}
	return nil, fmt.Errorf("revision %q does not belong to this %s", name, noun)
}

// RollbackTarget returns a new service definition with 100% of the live service's traffic sent
// to revision. It does not modify its argument; it shallow-copies only the Spec that has to
// change.
//
// spec.template is not touched, so no new revision is created. Tagged routes are kept at 0% (a
// rollback must not take away access through the tag URL). Existing untagged entries (including
// latestRevision) are dropped, because their traffic is consolidated onto the target.
func RollbackTarget(live *run.Service, revision string) (*run.Service, error) {
	if live == nil || live.Spec == nil {
		return nil, errors.New("the live service has no spec to roll back")
	}
	if revision == "" {
		return nil, errors.New("no revision to roll back to")
	}

	traffic := []*run.TrafficTarget{{RevisionName: revision, Percent: 100}}
	for _, t := range live.Spec.Traffic {
		if t == nil || t.Tag == "" {
			continue
		}
		kept := *t
		kept.Percent = 0
		traffic = append(traffic, &kept)
	}

	spec := *live.Spec
	spec.Traffic = traffic
	out := *live
	out.Spec = &spec
	return &out, nil
}

// unfinishedRolloutError explains why rollback will not pick a revision while the newest one is
// not Ready (see SelectRollbackRevision).
func unfinishedRolloutError(newest, serving Revision, words splitWords) error {
	if newest.Ready == conditionFalse {
		state := "failed"
		if newest.Reason != "" {
			state = fmt.Sprintf("failed (%s)", newest.Reason)
		}
		return fmt.Errorf("the newest revision %q %s and %s; %s still on %q, "+
			"the last revision that became ready, so there is nothing to roll back: fix the manifest "+
			"and deploy again, or pass --revision to choose a specific revision",
			newest.Name, state, words.idle, words.holder, serving.Name)
	}
	return fmt.Errorf("the newest revision %q is not ready yet and %s; %s still "+
		"on %q: wait for the rollout to finish (%s), or pass --revision to choose a "+
		"specific revision", newest.Name, words.idle, words.holder, serving.Name, words.wait)
}
