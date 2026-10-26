package cloudrun

import (
	"errors"
	"fmt"

	run "google.golang.org/api/run/v1"
)

// SplitRequest is what traffic (a service) or instance-split (a worker pool) was asked to do: send
// a share of what the resource splits between its revisions to one of them. Exactly one of Revision
// and Latest.
type SplitRequest struct {
	// Revision is the name of the revision to send the share to.
	Revision string
	// Latest sends the share to "the latest revision" (latestRevision: true).
	// It does not pin a revision name, so it automatically follows the versions later deploys
	// create.
	Latest bool
	// Percent is the share to send to the target (1..100). Below 100, the remainder stays on the
	// revision currently receiving the most.
	Percent int64
}

// ValidateSplitRequest checks the combination of options for a kind. It needs no API access, so
// it can be called before building the client (= before looking for credentials). cmd runs this
// first so that a flag mistake is not hidden behind an authentication error.
func ValidateSplitRequest(kind Kind, req SplitRequest) error {
	return kind.validateSplit(req)
}

func validateSplitRequest(req SplitRequest, words splitWords) error {
	switch {
	case req.Latest && req.Revision != "":
		return errors.New("--to and --to-latest cannot be combined")
	case !req.Latest && req.Revision == "":
		return fmt.Errorf("no revision to %s: pass --to or --to-latest", words.sendTo)
	case req.Percent <= 0 || req.Percent > 100:
		return fmt.Errorf("--percent must be between 1 and 100, got %d", req.Percent)
	}
	return nil
}

// ShiftTrafficTarget returns the live service's definition with only its traffic split rewritten
// (the name ends in Target, like RollbackTarget / RefreshTarget, to show that it is "a pure
// function that builds the desired definition to apply").
// It does not modify its argument; it shallow-copies only the Spec that has to change.
//
// spec.template is not touched, so no new revision is created. A canary (--percent 10) and
// returning to the latest after a rollback (--to-latest) can both be expressed through this one
// path.
//
// The remaining share goes to "the revision currently receiving the most". That is the shape a
// canary actually takes in practice (stable 90% / new 10%), and the result is always a split
// between two revisions, so what will happen can be read before running it. Keeping the existing
// split proportionally would need rounding errors handled, and it is hard to predict what is left.
func ShiftTrafficTarget(live *run.Service, req SplitRequest) (*run.Service, error) {
	if live == nil || live.Spec == nil {
		return nil, errors.New("the live service has no spec to update")
	}
	if err := validateSplitRequest(req, serviceSplitWords); err != nil {
		return nil, err
	}

	status := newStatus(live)

	target := &run.TrafficTarget{Percent: req.Percent}
	// The name used to exclude the target itself from the candidates when choosing where the
	// remainder goes.
	self := req.Revision
	if req.Latest {
		target.LatestRevision = true
		// latestRevision resolves to the newest *created* revision, as for a worker pool
		// (confirmed against the real API: after deploy --no-traffic, --to-latest moves the
		// traffic onto the revision just created). While that revision serves nothing,
		// latestReadyRevisionName can still name the stable one; excluding latestReady here would
		// refuse the canary as "already serving 100%".
		self = status.LatestCreatedRevision
	} else {
		target.RevisionName = req.Revision
	}

	traffic := []*run.TrafficTarget{target}
	if req.Percent < 100 {
		rest, err := remainderRevision(trafficByRevision(status), self, req.Percent, serviceSplitWords)
		if err != nil {
			return nil, err
		}
		traffic = append(traffic, &run.TrafficTarget{RevisionName: rest, Percent: 100 - req.Percent})
	}

	// Tagged routes are kept at 0%. Changing the split must not take away access through the tag
	// URL (the same treatment as rollback).
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

// checkLatestCanServe refuses --to-latest when the newest created revision, which latestRevision
// resolves to, has failed (Ready=False). Confirmed against the real API for a service: Cloud Run
// accepts such a split but routes nothing to the failed revision, the service reports Ready=False,
// and the command fails only after the apply has written latestRevision into spec.traffic.
//
// It catches a revision a deploy rolled out and saw fail. A revision that has never had a share is
// not started, so it reports Ready=True (Retired) even when its container cannot start, and a
// failing worker pool revision stayed Ready=Unknown for minutes (both confirmed against the real
// API); neither is known to have failed when the split is planned.
func checkLatestCanServe(revisions Revisions, latest string, words splitWords) error {
	for _, r := range revisions {
		if r.Name != latest || r.Ready != conditionFalse {
			continue
		}
		state := "failed"
		if r.Reason != "" {
			state = fmt.Sprintf("failed (%s)", r.Reason)
		}
		return fmt.Errorf("the latest revision %q %s, and --to-latest would %s it; deploy a fixed "+
			"manifest first, or pass --to to choose a ready revision", r.Name, state, words.sendTo)
	}
	return nil
}

// remainderRevision chooses the revision that keeps the remaining share: the revision currently
// receiving the most. If that is the target itself, there is nothing to split against, so it is
// an error.
//
// Do not choose "the largest *excluding* the target". Mid-canary (stable 90% / old 10%), running
// --percent 10 with the stable revision as the target would drop the stable revision to 10% and
// give the old one 90%, a split nobody wants. When the target already carries production, that is
// a case of "nothing to split against", not a case for promoting the runner-up.
func remainderRevision(shares map[string]revisionTraffic, target string, percent int64, words splitWords) (string, error) {
	name, share := largestShare(shares)
	switch name {
	case "":
		return "", fmt.Errorf(
			"no revision is %s %s to keep the remaining %d%%; pass --percent 100",
			words.verb, words.what, 100-percent)
	case target:
		return "", fmt.Errorf(
			"revision %q is already %s %d%% of the %s, so there is nothing to split it "+
				"against; pass --percent 100, or send the share to the other revision with --to",
			target, words.verb, share, words.what)
	}
	return name, nil
}

// largestShare returns the revision currently receiving the most, and its share.
// Ties are broken by name in ascending order (so the result does not change from run to run).
// If no revision is receiving any, the name is the empty string.
func largestShare(shares map[string]revisionTraffic) (string, int64) {
	best, bestShare := "", int64(0)
	for name, share := range shares {
		if share.percent <= 0 {
			continue
		}
		if share.percent > bestShare || (share.percent == bestShare && name < best) {
			best, bestShare = name, share.percent
		}
	}
	return best, bestShare
}

// pinnedTraffic returns the live service's *current* split as a spec.traffic pinned by revision
// name. It exists for deploy --no-traffic: left as latestRevision: true, all the traffic would
// move to the revision about to be created, so it is replaced with concrete names.
func pinnedTraffic(live *run.Service) []*run.TrafficTarget {
	status := newStatus(live)
	var out []*run.TrafficTarget
	for _, t := range status.Traffic {
		if t.RevisionName == "" {
			continue
		}
		out = append(out, &run.TrafficTarget{
			RevisionName: t.RevisionName,
			Tag:          t.Tag,
			Percent:      t.Percent,
		})
	}
	return out
}
