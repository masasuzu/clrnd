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
func SelectRollbackRevision(revisions Revisions, requested string) (*Revision, error) {
	if requested != "" {
		return FindRevision(revisions, requested)
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
		return nil, errors.New("no revision is currently receiving traffic; pass --revision to choose one")
	}

	for i := current + 1; i < len(revisions); i++ {
		if revisions[i].Ready == conditionTrue {
			return &revisions[i], nil
		}
	}
	return nil, fmt.Errorf("no ready revision older than %q to roll back to; pass --revision to choose one",
		revisions[current].Name)
}

// FindRevision returns the revision in the list whose name matches. It is an error if none does.
// It is also the check that "this is a revision of this service": sending traffic to a mistyped
// name would produce a split that reaches no revision at all.
func FindRevision(revisions Revisions, name string) (*Revision, error) {
	for i := range revisions {
		if revisions[i].Name == name {
			return &revisions[i], nil
		}
	}
	return nil, fmt.Errorf("revision %q does not belong to this service", name)
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
