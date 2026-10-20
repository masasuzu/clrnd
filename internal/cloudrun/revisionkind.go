package cloudrun

import (
	"context"
	"errors"
	"fmt"

	run "google.golang.org/api/run/v1"
)

// revisionsOf describes how a kind that has revisions (a service, a worker pool) finds them and
// what of its live definition bears on them. A kind without revisions (a job) leaves it nil.
//
// Revisions are listed from the revisions API by a label naming the resource, while what each one
// serves lives on the resource itself (a service's status.traffic, a worker pool's
// status.instanceSplits), so a listing joins the two.
type revisionsOf[T resource] struct {
	// label names the resource a revision belongs to.
	label string
	// shares returns what each revision serves now, by revision name.
	shares func(T) map[string]revisionTraffic
	// protected returns the revisions a prune must never delete (see protectedRevisions).
	protected func(T) map[string]bool
	// routeAllTo returns a copy of the live resource that sends everything to revision, for
	// rollback. It does not modify its argument.
	routeAllTo func(live T, revision string) (T, error)
	// shift returns a copy of the live resource with its split changed as req asks, for traffic
	// and instance-split. It does not modify its argument.
	shift func(live T, req SplitRequest) (T, error)
	// pinsSplit reports whether a manifest sets the split itself (spec.traffic,
	// spec.instanceSplits), which deploy's flag for keeping the current split replaces.
	pinsSplit func(T) bool
	// words are how messages put what the kind splits between revisions.
	words splitWords
}

// errNoRevisions is what a kind without revisions answers to a revisions command.
func (k *kindOf[T]) errNoRevisions() error {
	return fmt.Errorf("a %s has no revisions", k.noun)
}

// listRevisionsOf lists the revisions of a resource already read, joined with what it serves.
func (k *kindOf[T]) listRevisionsOf(ctx context.Context, c *Client, obj T) (Revisions, error) {
	if k.revisions == nil {
		return nil, k.errNoRevisions()
	}
	meta := k.meta(obj)
	if meta == nil || meta.Name == "" {
		return nil, fmt.Errorf("no %s to list the revisions of", k.noun)
	}
	name := meta.Name
	selector := fmt.Sprintf("%s=%s", k.revisions.label, name)
	items, err := listAll(fmt.Sprintf("revisions of %s %q", k.noun, name),
		func(token string) ([]*run.Revision, string, error) {
			call := c.api.Namespaces.Revisions.List(c.parent()).LabelSelector(selector).Limit(listPageLimit)
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
	return newRevisionsWithShares(items, k.revisions.shares(obj), k.revisions.protected(obj)), nil
}

func (k *kindOf[T]) listRevisions(ctx context.Context, c *Client, name string) (Revisions, error) {
	if k.revisions == nil {
		return nil, k.errNoRevisions()
	}
	obj, err := k.get(ctx, c, name)
	if err != nil {
		return nil, fmt.Errorf("failed to get %s %q: %w", k.noun, name, err)
	}
	return k.listRevisionsOf(ctx, c, obj)
}

// planRollback reads the resource once, picks the revision to return to from its revisions (see
// SelectRollbackRevision), and plans sending everything there against that same read. Only the
// split changes, so no revision is created.
func (k *kindOf[T]) planRollback(ctx context.Context, c *Client, name, requested string) (*DeployPlan, *Revision, error) {
	if k.revisions == nil {
		return nil, nil, k.errNoRevisions()
	}
	live, err := k.get(ctx, c, name)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get %s %q: %w", k.noun, name, err)
	}
	revisions, err := k.listRevisionsOf(ctx, c, live)
	if err != nil {
		return nil, nil, err
	}
	target, err := selectRollbackRevision(revisions, requested, k.revisions.words)
	if err != nil {
		return nil, nil, err
	}
	desired, err := k.revisions.routeAllTo(live, target.Name)
	if err != nil {
		return nil, nil, err
	}
	// desired comes from the live definition, so the defaults are already in it: no resolution.
	plan, err := k.planAgainst(ctx, c, name, live, desired, PlanOptions{}, name)
	if err != nil {
		return nil, nil, err
	}
	return plan, target, nil
}

func (k *kindOf[T]) validateSplit(req SplitRequest) error {
	if k.revisions == nil {
		return k.errNoRevisions()
	}
	return validateSplitRequest(req, k.revisions.words)
}

// planSplit reads the resource once, checks that a revision named in req is one of its own, and
// plans the split req asks for against that same read. Only the split changes, so no revision is
// created. It returns the revision named in req, so that the caller can warn when it is not Ready;
// nil for --to-latest.
func (k *kindOf[T]) planSplit(ctx context.Context, c *Client, name string, req SplitRequest) (*DeployPlan, *Revision, error) {
	if err := k.validateSplit(req); err != nil {
		return nil, nil, err
	}
	live, err := k.get(ctx, c, name)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get %s %q: %w", k.noun, name, err)
	}
	var target *Revision
	if req.Revision != "" {
		revisions, err := k.listRevisionsOf(ctx, c, live)
		if err != nil {
			return nil, nil, err
		}
		if target, err = findRevision(revisions, req.Revision, k.noun); err != nil {
			return nil, nil, err
		}
	}
	desired, err := k.revisions.shift(live, req)
	if err != nil {
		return nil, nil, err
	}
	// desired comes from the live definition, so the defaults are already in it: no resolution.
	plan, err := k.planAgainst(ctx, c, name, live, desired, PlanOptions{}, name)
	if err != nil {
		return nil, nil, err
	}
	return plan, target, nil
}

func (k *kindOf[T]) manifestPinsSplit(manifest []byte) (bool, error) {
	obj, err := k.parse(manifest)
	if err != nil {
		return false, err
	}
	return k.revisions != nil && k.revisions.pinsSplit(obj), nil
}

// ListKindRevisions returns the revisions of a resource of the given kind, newest first, with
// the share of what it splits each one serves. It fails for a kind without revisions.
func (c *Client) ListKindRevisions(ctx context.Context, kind Kind, name string) (Revisions, error) {
	return kind.listRevisions(ctx, c, name)
}

// PlanRollback plans sending everything a resource of the given kind serves to an earlier
// revision: requested, or the one before the newest serving revision when it is empty. It returns
// the plan and the chosen revision.
func (c *Client) PlanRollback(ctx context.Context, kind Kind, name, requested string) (*DeployPlan, *Revision, error) {
	return kind.planRollback(ctx, c, name, requested)
}

// PlanSplit plans sending a share of what a resource of the given kind splits between its
// revisions (a service's traffic, a worker pool's instances) as req asks. It returns the plan and
// the revision req names, nil for --to-latest.
func (c *Client) PlanSplit(ctx context.Context, kind Kind, name string, req SplitRequest) (*DeployPlan, *Revision, error) {
	return kind.planSplit(ctx, c, name, req)
}

// HasSplit reports whether the manifest sets the split itself: a service's spec.traffic, a worker
// pool's spec.instanceSplits. It is used to warn that keeping the current split on deploy replaces
// it (it only parses and does not use the API). It is false for a kind without revisions.
func HasSplit(kind Kind, manifest []byte) (bool, error) {
	return kind.manifestPinsSplit(manifest)
}

// serviceRevisions is the revisions hook of the service kind.
var serviceRevisions = &revisionsOf[*run.Service]{
	label:      serviceLabel,
	shares:     func(svc *run.Service) map[string]revisionTraffic { return trafficByRevision(newStatus(svc)) },
	protected:  pinnedRevisionNames,
	routeAllTo: RollbackTarget,
	shift:      ShiftTrafficTarget,
	pinsSplit:  func(svc *run.Service) bool { return svc.Spec != nil && len(svc.Spec.Traffic) > 0 },
	words:      serviceSplitWords,
}

// workerPoolLabel is the label naming the worker pool a revision belongs to (confirmed against the
// real API).
const workerPoolLabel = "run.googleapis.com/workerPool"

// workerPoolRevisions is the revisions hook of the worker pool kind: what it splits between
// revisions is its instances.
var workerPoolRevisions = &revisionsOf[*run.WorkerPool]{
	label:      workerPoolLabel,
	shares:     instancesByRevision,
	protected:  protectedWorkerPoolRevisions,
	routeAllTo: workerPoolRollbackTarget,
	shift:      shiftInstanceSplit,
	pinsSplit:  func(pool *run.WorkerPool) bool { return pool.Spec != nil && len(pool.Spec.InstanceSplits) > 0 },
	words:      workerPoolSplitWords,
}

// instancesByRevision gathers a worker pool's status.instanceSplits per revision. The status
// always names the revision.
func instancesByRevision(pool *run.WorkerPool) map[string]revisionTraffic {
	out := make(map[string]revisionTraffic)
	if pool == nil || pool.Status == nil {
		return out
	}
	for _, split := range pool.Status.InstanceSplits {
		if split == nil || split.RevisionName == "" {
			continue
		}
		current := out[split.RevisionName]
		current.percent += split.Percent
		out[split.RevisionName] = current
	}
	return out
}

// protectedWorkerPoolRevisions is pinnedRevisionNames for a worker pool's spec.instanceSplits.
func protectedWorkerPoolRevisions(pool *run.WorkerPool) map[string]bool {
	if pool == nil || pool.Spec == nil {
		return map[string]bool{}
	}
	var named []string
	followsLatest := false
	for _, split := range pool.Spec.InstanceSplits {
		if split == nil {
			continue
		}
		named = append(named, split.RevisionName)
		followsLatest = followsLatest || split.LatestRevision
	}
	var latestReady, latestCreated string
	if pool.Status != nil {
		latestReady, latestCreated = pool.Status.LatestReadyRevisionName, pool.Status.LatestCreatedRevisionName
	}
	return protectedRevisions(named, followsLatest, latestReady, latestCreated)
}

// workerPoolRollbackTarget returns a copy of the live worker pool running every instance on
// revision. It does not modify its argument; it shallow-copies only the Spec. spec.template is not
// touched, so no revision is created (confirmed against the real API: the old revision, retired
// but still Ready, takes the instances back).
func workerPoolRollbackTarget(live *run.WorkerPool, revision string) (*run.WorkerPool, error) {
	if live == nil || live.Spec == nil {
		return nil, errors.New("the live worker pool has no spec to roll back")
	}
	if revision == "" {
		return nil, errors.New("no revision to roll back to")
	}
	spec := *live.Spec
	spec.InstanceSplits = []*run.InstanceSplit{{RevisionName: revision, Percent: 100}}
	out := *live
	out.Spec = &spec
	return &out, nil
}
