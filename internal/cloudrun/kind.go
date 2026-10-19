package cloudrun

import (
	"context"
	"errors"
	"fmt"

	run "google.golang.org/api/run/v1"
	"sigs.k8s.io/yaml"
)

// Kind is a kind of Cloud Run resource that clrnd deploys from a manifest: KindService or KindJob.
//
// Parsing and validating a manifest, comparing it with what is live, and applying it work the same
// way for every kind — confirmed against the real API for jobs: the same resourceVersion
// compare-and-swap, and a dry run that returns the server's defaults. What differs (the API calls,
// the manifest's apiVersion, where the containers sit, what the comparison has to align) is
// described once per kind by a kindOf, and everything else is written against this interface.
// Its methods are unexported, so no other package can add a kind.
type Kind interface {
	// Name is the manifest's kind field: "Service" or "Job".
	Name() string
	// Noun is what messages call one: "service" or "job".
	Noun() string

	validateManifest(manifest []byte, name string) error
	checkSyntax(manifest []byte) error
	overrideImages(manifest []byte, overrides []ImageOverride) ([]byte, error)
	manifestWorkload(manifest []byte) (workload, error)
	plan(ctx context.Context, c *Client, name string, manifest []byte, opts PlanOptions) (*DeployPlan, error)
	compare(ctx context.Context, c *Client, name string, manifest []byte, desiredLabel string,
		opts PlanOptions) (string, error)
	apply(ctx context.Context, p *DeployPlan, dryRun bool) (int64, error)
	wait(ctx context.Context, c *Client, name string, opts WaitOptions) (*Status, error)
	liveManifest(ctx context.Context, c *Client, name string) ([]byte, error)
	remove(ctx context.Context, c *Client, name string, dryRun bool) error
	waitDeleted(ctx context.Context, c *Client, name string, opts WaitOptions) error
}

// The kinds clrnd deploys.
var (
	KindService Kind = serviceKind
	KindJob     Kind = jobKind
)

// resource is the API type of a kind.
type resource interface {
	*run.Service | *run.Job
}

// kindOf describes one kind. The plan / diff / apply flow is written once on top of it.
type kindOf[T resource] struct {
	name       string // the manifest's kind field
	noun       string // what messages call one ("service", "job")
	apiVersion string // the apiVersion its manifest must declare

	// conflictHint is added to the explanation of a 409 on apply. Empty adds nothing.
	conflictHint string

	newObject func() T
	typeMeta  func(T) (apiVersion, kind string)
	meta      func(T) *run.ObjectMeta
	workload  func(T) workload
	status    func(T) *Status

	// explicit adjusts a strictly parsed manifest for what only the raw manifest can tell: whether
	// a field was written at all. A zero value and an absent field parse to the same thing. nil
	// adds nothing.
	explicit func(manifest []byte, obj T) error
	// retype rewrites a manifest the strict parse rejected into one it accepts, for a value written
	// in a form the generated type does not take (a number where it wants a string). It returns
	// nil bytes when it changed nothing, and the original error stands, or an error when what it
	// found is the real problem. It only runs after a failure, so a manifest that parses as written
	// is never rewritten. nil adds nothing.
	retype func(manifest []byte) ([]byte, error)

	// The API calls. They return the API's error as is; the callers add what they were doing.
	get     func(ctx context.Context, c *Client, name string) (T, error)
	create  func(ctx context.Context, c *Client, obj T, dryRun bool) (T, error)
	replace func(ctx context.Context, c *Client, name string, obj T, dryRun bool) (T, error)
	del     func(ctx context.Context, c *Client, name string, dryRun bool) error

	// prepare adjusts the desired definition before it is diffed and applied; current is the zero
	// value when the resource does not exist yet. nil leaves desired as it is.
	prepare func(desired, current T, name string, opts PlanOptions) (T, error)
	// align adjusts the live definition for the comparison only (never for the apply). It is not
	// called when there is no live definition. nil leaves it as it is.
	align func(current, desired T) T
	// scaffold adjusts a live definition before init writes it out as a manifest. nil leaves it as
	// it is.
	scaffold func(T) T
}

// Name implements Kind.
func (k *kindOf[T]) Name() string { return k.name }

// Noun implements Kind.
func (k *kindOf[T]) Noun() string { return k.noun }

// parse parses a manifest strictly; UnmarshalStrict also catches unknown fields (a misspelled
// field name and the like).
//
// A manifest of another kind clrnd knows is reported as such first. Parsed strictly as this kind,
// a job manifest would otherwise fail on whichever field a service does not have, and the message
// would point at that field instead of at the command.
func (k *kindOf[T]) parse(manifest []byte) (T, error) {
	var zero T
	if err := checkManifestKind(manifest, k.name); err != nil {
		return zero, err
	}
	obj := k.newObject()
	if err := yaml.UnmarshalStrict(manifest, obj); err != nil {
		var retyped []byte
		if k.retype != nil {
			var rerr error
			if retyped, rerr = k.retype(manifest); rerr != nil {
				return zero, fmt.Errorf("failed to parse the manifest: %w", rerr)
			}
		}
		if retyped == nil {
			return zero, fmt.Errorf("failed to parse the manifest: %w", err)
		}
		obj = k.newObject()
		if err := yaml.UnmarshalStrict(retyped, obj); err != nil {
			return zero, fmt.Errorf("failed to parse the manifest: %w", err)
		}
		manifest = retyped
	}
	if k.explicit != nil {
		if err := k.explicit(manifest, obj); err != nil {
			return zero, err
		}
	}
	// A manifest never carries a version to compare against: the version comes from the live read
	// the diff is computed from (setResourceVersion). One left in — a manifest made from
	// `gcloud ... describe` keeps it — would otherwise be sent as is, and every write would fail
	// with a 409 that no re-run fixes, while the diff, which never shows the field, looked clean.
	if meta := k.meta(obj); meta != nil {
		meta.ResourceVersion = ""
	}
	return obj, nil
}

// validate checks a parsed definition: what every kind needs to be deployable, and that it
// describes the resource the command was given.
func (k *kindOf[T]) validate(obj T, name string) error {
	var errs []error
	apiVersion, kind := k.typeMeta(obj)
	if apiVersion != k.apiVersion {
		errs = append(errs, fmt.Errorf("apiVersion must be %q, got %q", k.apiVersion, apiVersion))
	}
	if kind != k.name {
		errs = append(errs, fmt.Errorf("kind must be %q, got %q", k.name, kind))
	}

	switch meta := k.meta(obj); {
	case meta == nil || meta.Name == "":
		errs = append(errs, errors.New("metadata.name is required"))
	case meta.Name != name:
		errs = append(errs, fmt.Errorf("metadata.name %q does not match %s argument %q", meta.Name, k.noun, name))
	}

	errs = append(errs, validateContainers(k.workload(obj))...)
	return errors.Join(errs...)
}

func (k *kindOf[T]) validateManifest(manifest []byte, name string) error {
	obj, err := k.parse(manifest)
	if err != nil {
		return err
	}
	return k.validate(obj, name)
}

func (k *kindOf[T]) checkSyntax(manifest []byte) error {
	_, err := k.parse(manifest)
	return err
}

func (k *kindOf[T]) manifestWorkload(manifest []byte) (workload, error) {
	obj, err := k.parse(manifest)
	if err != nil {
		return workload{}, err
	}
	return k.workload(obj), nil
}

func (k *kindOf[T]) overrideImages(manifest []byte, overrides []ImageOverride) ([]byte, error) {
	obj, err := k.parse(manifest)
	if err != nil {
		return nil, err
	}
	if err := applyImageOverrides(k.workload(obj), overrides); err != nil {
		return nil, err
	}
	out, err := yaml.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("failed to rebuild the manifest after --image: %w", err)
	}
	return out, nil
}

func (k *kindOf[T]) plan(ctx context.Context, c *Client, name string, manifest []byte,
	opts PlanOptions) (*DeployPlan, error) {
	obj, err := k.parse(manifest)
	if err != nil {
		return nil, err
	}
	if err := k.validate(obj, name); err != nil {
		return nil, err
	}
	return k.planObject(ctx, c, name, obj, opts)
}

// lookup fetches the live definition. A resource that does not exist is not an error: it comes
// back as the zero value, which the callers treat as "everything is an addition".
func (k *kindOf[T]) lookup(ctx context.Context, c *Client, name string) (T, error) {
	obj, err := k.get(ctx, c, name)
	if err != nil {
		var zero T
		if isNotFound(err) {
			return zero, nil
		}
		return zero, err
	}
	return obj, nil
}

// planObject computes the diff against live using the desired definition as is. It is the
// entry point for paths that edit live and apply it without going through a manifest, such as
// rollback and refresh. The desired metadata.namespace is rewritten to match the target.
func (k *kindOf[T]) planObject(ctx context.Context, c *Client, name string, desired T,
	opts PlanOptions) (*DeployPlan, error) {
	var zero T
	if desired == zero {
		return nil, fmt.Errorf("no desired %s to plan", k.noun)
	}
	c.setNamespace(k.meta(desired))

	current, err := k.lookup(ctx, c, name)
	if err != nil {
		return nil, fmt.Errorf("failed to check %s %q: %w", k.noun, name, err)
	}
	create := current == zero

	// Anything that rewrites desired (pinning the traffic for --no-traffic) happens before setting
	// resourceVersion and before diffing. Both what is applied and what shows in the diff have to
	// be the definition after it.
	if k.prepare != nil {
		if desired, err = k.prepare(desired, current, name, opts); err != nil {
			return nil, err
		}
	}

	plan := &DeployPlan{Name: name, Create: create, client: c, kind: k, desired: desired}

	// For an update, put the resourceVersion of the state just read onto desired to make the write
	// a compare-and-swap. Sent without it, Cloud Run accepts the write as an unconditional
	// overwrite (confirmed against the real API, for services and jobs alike), so concurrent
	// deploys silently erase each other's changes. A definition that was itself read from live
	// before this GET (rollback, refresh, traffic) already carries the version of that read, and
	// setResourceVersion keeps it: a change made in between then fails with a 409 instead of being
	// overwritten.
	if !create {
		setResourceVersion(k.meta(desired), k.meta(current))
	}

	// Resolve defaults only on the desired definition used for the diff. plan.desired (what is sent
	// for the apply) is left as it was, so values the server filled in are not written back.
	compared := desired
	if opts.ResolveDefaults {
		if compared, err = k.resolveDefaults(ctx, c, name, desired, create); err != nil {
			return nil, err
		}
	}

	if plan.Diff, err = k.compareObjects(current, compared, "live/"+name, name); err != nil {
		return nil, err
	}
	return plan, nil
}

func (k *kindOf[T]) compare(ctx context.Context, c *Client, name string, manifest []byte,
	desiredLabel string, opts PlanOptions) (string, error) {
	desired, err := k.parse(manifest)
	if err != nil {
		return "", err
	}
	// Run the same validation as deploy, whether or not server defaults are resolved. Doing it only
	// on the dry-run path would let diff --no-server-defaults accept input deploy rejects
	// (metadata.name differing from the name given, etc.) and render "a diff that looks as if the
	// name could be changed".
	if err := k.validate(desired, name); err != nil {
		return "", err
	}
	// Match the target. The dry run that resolves server defaults gets the same validation as a
	// real write, so unless the same pre-processing as deploy is applied, diff alone gets rejected.
	c.setNamespace(k.meta(desired))

	// One not created yet is "everything is an addition", like planObject. A 404 used to be
	// returned here, so the "write a manifest → diff → deploy" flow the README recommends before
	// init failed on the first run only.
	current, err := k.lookup(ctx, c, name)
	if err != nil {
		return "", fmt.Errorf("failed to get %s %q: %w", k.noun, name, err)
	}

	if opts.ResolveDefaults {
		var zero T
		// If it does not exist, the dry run is also a 404 unless it is a create.
		if desired, err = k.resolveDefaults(ctx, c, name, desired, current == zero); err != nil {
			return "", err
		}
	}
	return k.compareObjects(current, desired, "live/"+name, desiredLabel)
}

// compareObjects is the comparison every diff goes through. It does not modify its arguments. A
// zero current means the resource does not exist yet, so everything is an addition.
func (k *kindOf[T]) compareObjects(current, desired T, currentName, desiredName string) (string, error) {
	var zero T
	exists := current != zero
	if exists && k.align != nil {
		current = k.align(current, desired)
	}

	desiredYAML, err := toManifest(desired)
	if err != nil {
		return "", err
	}
	var currentYAML []byte
	if exists {
		if currentYAML, err = toManifest(current); err != nil {
			return "", err
		}
	}
	return Diff(currentYAML, desiredYAML, currentName, desiredName)
}

// resolveDefaults runs a server-side dry run to get a definition that includes the defaults Cloud
// Run fills in. It changes nothing (dryRun=all).
func (k *kindOf[T]) resolveDefaults(ctx context.Context, c *Client, name string, desired T,
	create bool) (T, error) {
	var (
		resolved T
		err      error
	)
	if create {
		resolved, err = k.create(ctx, c, desired, true)
	} else {
		resolved, err = k.replace(ctx, c, name, desired, true)
	}
	if err != nil {
		// The cause is not necessarily permissions (the manifest's content was rejected, the
		// resource was deleted midway, etc.). Do not assert one; only add what this path is doing.
		// But create and update need different permissions, so name the one actually called. For a
		// diff against something that does not exist, saying "update permission is needed" would
		// leave someone who should add the create permission unable to ever fix it.
		call := "update"
		if create {
			call = "create"
		}
		var zero T
		return zero, fmt.Errorf(
			"failed to resolve server defaults for %s %q: %w "+
				"(resolving them performs a dry-run %s, which needs permission to %s the "+
				"%s; pass --no-server-defaults to compare without one)", k.noun, name, err, call, call, k.noun)
	}
	return resolved, nil
}

func (k *kindOf[T]) apply(ctx context.Context, p *DeployPlan, dryRun bool) (int64, error) {
	desired, ok := p.desired.(T)
	if !ok {
		return 0, fmt.Errorf("internal error: the plan for %s %q holds a %T", k.noun, p.Name, p.desired)
	}
	if p.Create {
		applied, err := k.create(ctx, p.client, desired, dryRun)
		if err != nil {
			return 0, fmt.Errorf("failed to create %s %q: %w", k.noun, p.Name, err)
		}
		return generationOf(k.meta(applied)), nil
	}

	applied, err := k.replace(ctx, p.client, p.Name, desired, dryRun)
	if err != nil {
		if isConflict(err) {
			// resourceVersion is sent, so a 409 is the case where "someone rewrote it between
			// computing the diff and applying". The API's wording (version 'X' was specified but
			// current version is 'Y') alone does not say what to do, so rephrase it.
			return 0, fmt.Errorf("%s %q changed after the diff was computed%s; re-run to compare "+
				"against the current state: %w", k.noun, p.Name, k.conflictHint, err)
		}
		return 0, fmt.Errorf("failed to update %s %q: %w", k.noun, p.Name, err)
	}
	return generationOf(k.meta(applied)), nil
}

func (k *kindOf[T]) wait(ctx context.Context, c *Client, name string, opts WaitOptions) (*Status, error) {
	return waitReady(ctx, subject(k.noun, name), func(ctx context.Context) (*Status, error) {
		obj, err := k.get(ctx, c, name)
		if err != nil {
			return nil, fmt.Errorf("failed to get %s %q: %w", k.noun, name, err)
		}
		return k.status(obj), nil
	}, opts)
}

// generationOf extracts metadata.generation nil-safely. 0 means unknown, in which case a wait
// looks only at Ready regardless of generation.
func generationOf(meta *run.ObjectMeta) int64 {
	if meta == nil {
		return 0
	}
	return meta.Generation
}

// kindHints tells a command given a manifest of another kind where that manifest belongs.
var kindHints = map[string]string{
	"Service":    "a Cloud Run service manifest; use the top-level commands such as 'clrnd deploy'",
	"Job":        "a Cloud Run job manifest; use the 'clrnd jobs' commands",
	"WorkerPool": "a Cloud Run worker pool manifest, which clrnd does not support yet",
}

// checkManifestKind reports a manifest whose kind is one clrnd knows but not the one wanted.
// Anything else — a manifest it cannot read, no kind, a kind it has never heard of — is left to
// the strict parse and to validate, which already explain those.
func checkManifestKind(manifest []byte, want string) error {
	var head struct {
		Kind string `json:"kind"`
	}
	if err := yaml.Unmarshal(manifest, &head); err != nil || head.Kind == want {
		return nil
	}
	if hint, ok := kindHints[head.Kind]; ok {
		return fmt.Errorf("kind is %q, but this command takes a %s: this is %s", head.Kind, want, hint)
	}
	return nil
}

// DeployPlan is what is about to be applied. Computed by Plan and applied by Apply.
type DeployPlan struct {
	Name   string // the service or job name
	Create bool   // whether it does not exist and will be created
	Diff   string // unified diff of live and desired (empty when there are no differences)

	client  *Client
	kind    Kind
	desired any // the definition Apply sends, of kind's API type
}

// Project returns the project the plan applies to.
func (p *DeployPlan) Project() string { return p.client.project }

// Region returns the region the plan applies to.
func (p *DeployPlan) Region() string { return p.client.region }

// Noun returns what messages call the plan's resource: "service" or "job".
func (p *DeployPlan) Noun() string { return p.kind.Noun() }

// Apply applies the plan to Cloud Run and returns the metadata.generation it applied (0 when the
// response does not carry one). When dryRun is true, the server only validates. When dryRun is
// false, DryRun is not called (passing an empty string would send an empty dryRun= query
// parameter).
//
// The generation is "the generation just applied", so Wait can use it to wait only for that
// generation.
func (p *DeployPlan) Apply(ctx context.Context, dryRun bool) (int64, error) {
	return p.kind.apply(ctx, p, dryRun)
}

// Wait polls until what the plan applies settles (see Client.Wait). It works whether or not the
// plan has been applied: with no Generation it only checks that it is Ready now.
func (p *DeployPlan) Wait(ctx context.Context, opts WaitOptions) (*Status, error) {
	return p.kind.wait(ctx, p.client, p.Name, opts)
}

// PlanOptions holds optional settings for how the diff is computed. The zero value is the default
// behaviour.
type PlanOptions struct {
	// ResolveDefaults, when true, runs the desired definition through a server-side dry run before
	// diffing, to build a desired definition with the defaults filled in. Cloud Run fills defaults
	// into many fields on create, so a hand-written minimal manifest keeps showing a diff even when
	// nothing was changed (issue #11). Enabling this makes those differences line up on both sides
	// and disappear.
	//
	// A dry run is a write API, so it cannot be used with read-only permissions. The CLI enables
	// this by default and lets --no-server-defaults turn it off (the zero value of this struct
	// stays "do not resolve", and rollback / refresh, which work on a definition taken from live,
	// do not need resolving because the defaults are already in it).
	// What is sent for the apply is always the original desired definition; values the server
	// filled in are never written back.
	ResolveDefaults bool

	// KeepTraffic, when true, pins the current split by revision name before applying
	// (deploy --no-traffic). Without spec.traffic in the manifest, Cloud Run sends all traffic to
	// latestRevision, so left as is, the new revision would take production the moment it is
	// deployed. Set this when traffic is meant to be moved gradually. Services only.
	KeepTraffic bool
}

// Plan validates the manifest and computes the diff against what is live (changes nothing).
func (c *Client) Plan(ctx context.Context, kind Kind, name string, manifest []byte,
	opts PlanOptions) (*DeployPlan, error) {
	return kind.plan(ctx, c, name, manifest, opts)
}

// PlanService computes the diff against live using the desired service definition as is.
// It is the entry point for paths that edit live and apply it without going through a manifest,
// such as rollback and refresh. The desired metadata.namespace is rewritten to match the target.
func (c *Client) PlanService(ctx context.Context, service string, desired *run.Service,
	opts PlanOptions) (*DeployPlan, error) {
	return serviceKind.planObject(ctx, c, service, desired, opts)
}

// CompareManifest returns the diff between what is live and a local manifest. It is the entry
// point for diff, and does the fetch and (when needed) the default resolution together.
func (c *Client) CompareManifest(ctx context.Context, kind Kind, name string, manifest []byte,
	desiredLabel string, opts PlanOptions) (string, error) {
	return kind.compare(ctx, c, name, manifest, desiredLabel, opts)
}

// CheckSyntax checks only that the manifest parses strictly as the kind. It is used to surface
// local problems before anything that needs the API (unlike Validate, it does not look at the
// name or the required fields).
func CheckSyntax(kind Kind, manifest []byte) error {
	return kind.checkSyntax(manifest)
}

// Validate checks that a local manifest is a valid definition of the kind. It does not access the
// API and checks only the structure and the fields required to deploy. It returns nil when there
// is no problem, and an error combining all of them when there are several.
func Validate(kind Kind, manifest []byte, name string) error {
	return kind.validateManifest(manifest, name)
}

// setNamespace makes the body's namespace match the target project.
// Both the apply and the dry run use a definition that has been through this.
func (c *Client) setNamespace(meta *run.ObjectMeta) {
	if meta != nil {
		meta.Namespace = c.project
	}
}

// setResourceVersion copies current's resourceVersion onto desired so that optimistic concurrency
// control takes effect.
func setResourceVersion(desired, current *run.ObjectMeta) {
	if desired == nil || current == nil {
		return
	}
	// If desired already carries a version, do not overwrite it. rollback / refresh / traffic read
	// live and then edit it, so desired carries the version *of that read*. Replacing it with the
	// newer version here would let the CAS wave through someone else's change made between the
	// two GETs and silently revert it (meaning to touch only traffic, the image from the deploy
	// just before would be rolled back too). Sent with the version it carries, that case becomes a
	// 409.
	if desired.ResourceVersion != "" {
		return
	}
	desired.ResourceVersion = current.ResourceVersion
}

func (k *kindOf[T]) liveManifest(ctx context.Context, c *Client, name string) ([]byte, error) {
	obj, err := k.get(ctx, c, name)
	if err != nil {
		return nil, fmt.Errorf("failed to get %s %q: %w", k.noun, name, err)
	}
	if k.scaffold != nil {
		obj = k.scaffold(obj)
	}
	return toManifest(obj)
}

func (k *kindOf[T]) remove(ctx context.Context, c *Client, name string, dryRun bool) error {
	if err := k.del(ctx, c, name, dryRun); err != nil {
		return fmt.Errorf("failed to delete %s %q: %w", k.noun, name, err)
	}
	return nil
}

// LiveManifest fetches the live definition and returns it as a manifest usable for deploying:
// server-managed fields stripped, and adjusted the way init scaffolds it (a service loses its
// revision name, see WithoutRevisionName).
func (c *Client) LiveManifest(ctx context.Context, kind Kind, name string) ([]byte, error) {
	return kind.liveManifest(ctx, c, name)
}

// Delete deletes the service or job. When dryRun is true, the server only validates.
// This cannot be undone, so the caller must obtain confirmation. Deletion is asynchronous; see
// WaitDeleted.
func (c *Client) Delete(ctx context.Context, kind Kind, name string, dryRun bool) error {
	return kind.remove(ctx, c, name, dryRun)
}

// WaitDeleted waits until the service or job is actually gone. Cloud Run deletion is
// asynchronous — confirmed for jobs as well as services — and the resource can still be fetched
// when the DELETE is accepted, so without this a procedure such as "delete, then recreate" races.
func (c *Client) WaitDeleted(ctx context.Context, kind Kind, name string, opts WaitOptions) error {
	return kind.waitDeleted(ctx, c, name, opts)
}
