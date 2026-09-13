// Package cloudrun provides access to the Cloud Run Admin API and manifest normalization.
package cloudrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/pmezard/go-difflib/difflib"
	"google.golang.org/api/googleapi"
	run "google.golang.org/api/run/v1"
	"sigs.k8s.io/yaml"
)

const (
	manifestAPIVersion = "serving.knative.dev/v1"
	manifestKind       = "Service"
	// dryRunAll is the value of the API's dryRun query parameter that requests "validate only".
	dryRunAll = "all"
)

// Read-only annotations added by the server. A manifest for deploying does not need them.
// Applied to top-level metadata and to spec.template.metadata.
//
// client-name / client-version record "the tool that last wrote", not configuration. Importing a
// service created with gcloud through init would bake "gcloud" into the manifest, and every later
// clrnd deploy would keep sending it back. With a hand-written manifest it shows up the other way
// round, as a removal diff that never goes away. clrnd does not manage these.
var serverManagedAnnotations = []string{
	"run.googleapis.com/operation-id",
	"run.googleapis.com/ingress-status",
	"run.googleapis.com/urls",
	"run.googleapis.com/client-name",
	"run.googleapis.com/client-version",
	"serving.knative.dev/creator",
	"serving.knative.dev/lastModifier",
}

// Read-only labels added by the server. Applied to top-level metadata and to
// spec.template.metadata (cloud.googleapis.com/location is in practice only set on top-level
// metadata, but removing it from the template side does no harm, so the lists are not split).
var serverManagedLabels = []string{
	"client.knative.dev/nonce",
	"run.googleapis.com/startupProbeType",
	"cloud.googleapis.com/location",
}

// Read-only fields of top-level metadata. Lists the parts of run.ObjectMeta that clients do not
// write (or where writing them has no effect). This keeps them from being left in the scaffolded
// manifest, and sent back as is on the next deploy, when init imports a service that is being
// deleted or one managed by another controller (Terraform, Config Connector, etc.).
var serverManagedMetaFields = []string{
	"creationTimestamp",
	"generation",
	"resourceVersion",
	"selfLink",
	"uid",
	"namespace",
	"deletionTimestamp",
	"deletionGracePeriodSeconds",
	"finalizers",
	"ownerReferences",
	"generateName",
	"clusterName",
}

// DeployPlan is what is about to be applied. Computed by Plan and applied by Apply.
type DeployPlan struct {
	Service string // service name
	Create  bool   // whether the service does not exist and will be created
	Diff    string // unified diff of live and desired (empty when there are no differences)

	client  *Client
	desired *run.Service
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
	// deployed. Set this when traffic is meant to be moved gradually.
	KeepTraffic bool
}

// Plan validates the manifest and computes the diff against the live service (changes nothing).
func (c *Client) Plan(ctx context.Context, service string, manifest []byte, opts PlanOptions) (*DeployPlan, error) {
	svc, err := parseManifest(manifest)
	if err != nil {
		return nil, err
	}
	if err := validate(svc, service); err != nil {
		return nil, err
	}
	return c.PlanService(ctx, service, svc, opts)
}

// PlanService computes the diff against live using the desired service definition as is.
// It is the entry point for paths that edit live and apply it without going through a manifest,
// such as rollback and refresh. The desired metadata.namespace is rewritten to match the target.
func (c *Client) PlanService(ctx context.Context, service string, desired *run.Service, opts PlanOptions) (*DeployPlan, error) {
	if desired == nil {
		return nil, errors.New("no desired service to plan")
	}
	c.setNamespace(desired)

	create := false
	current, getErr := c.api.Namespaces.Services.Get(c.serviceName(service)).Context(ctx).Do()
	if getErr != nil {
		if !isNotFound(getErr) {
			return nil, fmt.Errorf("failed to check service %q: %w", service, getErr)
		}
		// Does not exist: create it. The diff is taken with the current side empty.
		create = true
		current = nil
	}

	// Pin the traffic before setting resourceVersion and before diffing. Both what is applied and
	// what shows in the diff have to be the definition after pinning.
	if opts.KeepTraffic {
		fixed, err := keepTraffic(desired, current, service)
		if err != nil {
			return nil, err
		}
		desired = fixed
	}

	plan := &DeployPlan{Service: service, client: c, desired: desired, Create: create}

	// For an update, put the resourceVersion of the state just read onto desired to make the write
	// a compare-and-swap. Sent without it, Cloud Run accepts the write as an unconditional
	// overwrite (confirmed against the real API), so concurrent deploys silently erase each
	// other's changes. A definition that was itself read from live before this GET (rollback,
	// refresh, traffic) already carries the version of that read, and setResourceVersion keeps it:
	// a change made in between then fails with a 409 instead of being overwritten.
	setResourceVersion(desired, current)

	// Resolve defaults only on the desired definition used for the diff. plan.desired (what is sent
	// for the apply) is left as it was, so values the server filled in are not written back.
	compared := desired
	if opts.ResolveDefaults {
		resolved, err := c.resolveDefaults(ctx, service, desired, create)
		if err != nil {
			return nil, err
		}
		compared = resolved
	}

	diff, err := compareServices(current, compared, "live/"+service, service)
	if err != nil {
		return nil, err
	}
	plan.Diff = diff
	return plan, nil
}

// CompareManifest returns the diff between the live service and a local manifest. It is the entry
// point for diff, and does the service fetch and (when needed) the default resolution together.
func (c *Client) CompareManifest(ctx context.Context, service string, manifest []byte,
	desiredLabel string, opts PlanOptions) (string, error) {
	desired, err := parseManifest(manifest)
	if err != nil {
		return "", err
	}
	// Run the same validation as deploy, whether or not server defaults are resolved. Doing it only
	// on the dry-run path would let diff --no-server-defaults accept input deploy rejects
	// (metadata.name differing from the service name, etc.) and render "a diff that looks as if the
	// name could be changed".
	if err := validate(desired, service); err != nil {
		return "", err
	}
	// Match the target. The dry run that resolves server defaults gets the same validation as a
	// real write, so unless the same pre-processing as deploy is applied, diff alone gets rejected.
	c.setNamespace(desired)

	current, err := c.GetService(ctx, service)
	if err != nil {
		if !isNotFound(err) {
			return "", err
		}
		// A service not created yet. Treated as "everything is an addition", like PlanService.
		// A 404 used to be returned here, so the "write a manifest → diff → deploy" flow the
		// README recommends before init failed on the first run only.
		current = nil
	}

	if opts.ResolveDefaults {
		// If it does not exist, the dry run is also a 404 unless it is a Create.
		if desired, err = c.resolveDefaults(ctx, service, desired, current == nil); err != nil {
			return "", err
		}
	}
	return compareServices(current, desired, "live/"+service, desiredLabel)
}

// keepTraffic replaces the desired traffic split with the live service's current split.
// It does not modify its arguments; it returns a new definition with only the Spec shallow-copied.
//
// "Do not send traffic to the new revision" can only be expressed by pinning the current split by
// name. It cannot be written in the manifest: the revision name is not known until the apply, and
// with latestRevision the new version would take all the traffic.
func keepTraffic(desired, current *run.Service, service string) (*run.Service, error) {
	if current == nil {
		return nil, fmt.Errorf("cannot keep traffic: service %q does not exist yet, so the "+
			"first revision has to receive it", service)
	}
	pinned := pinnedTraffic(current)
	if len(pinned) == 0 {
		return nil, fmt.Errorf("cannot keep traffic: service %q is not serving any revision yet", service)
	}
	if desired.Spec == nil {
		return nil, errors.New("the manifest has no spec to update")
	}
	spec := *desired.Spec
	spec.Traffic = pinned
	out := *desired
	out.Spec = &spec
	return &out, nil
}

// setNamespace makes the body's namespace match the target project.
// Both the apply and the dry run use a definition that has been through this.
func (c *Client) setNamespace(svc *run.Service) {
	if svc != nil && svc.Metadata != nil {
		svc.Metadata.Namespace = c.project
	}
}

// resolveDefaults runs a server-side dry run to get a service definition that includes the
// defaults Cloud Run fills in. It changes nothing (dryRun=all).
func (c *Client) resolveDefaults(ctx context.Context, service string, desired *run.Service, create bool) (*run.Service, error) {
	var (
		resolved *run.Service
		err      error
	)
	if create {
		resolved, err = c.api.Namespaces.Services.Create(c.parent(), desired).
			DryRun(dryRunAll).Context(ctx).Do()
	} else {
		resolved, err = c.api.Namespaces.Services.ReplaceService(c.serviceName(service), desired).
			DryRun(dryRunAll).Context(ctx).Do()
	}
	if err != nil {
		// The cause is not necessarily permissions (the manifest's content was rejected, the
		// service was deleted midway, etc.). Do not assert one; only add what this path is doing.
		// But create and update need different permissions, so name the one actually called. For a
		// diff against a service that does not exist, saying "update permission is needed" would
		// leave someone who should add run.services.create unable to ever fix it.
		call := "update"
		if create {
			call = "create"
		}
		return nil, fmt.Errorf(
			"failed to resolve server defaults for service %q: %w "+
				"(resolving them performs a dry-run %s, which needs permission to %s the "+
				"service; pass --no-server-defaults to compare without one)", service, err, call, call)
	}
	return resolved, nil
}

// Apply applies the Plan to Cloud Run and returns the service after the apply. When dryRun is
// true, the server only validates. When dryRun is false, DryRun is not called (passing an empty
// string would send an empty dryRun= query parameter).
//
// The returned metadata.generation is "the generation just applied", so it can be used to have
// Wait wait only for the rollout of that generation.
func (p *DeployPlan) Apply(ctx context.Context, dryRun bool) (*run.Service, error) {
	if p.Create {
		call := p.client.api.Namespaces.Services.Create(p.client.parent(), p.desired)
		if dryRun {
			call = call.DryRun(dryRunAll)
		}
		applied, err := call.Context(ctx).Do()
		if err != nil {
			return nil, fmt.Errorf("failed to create service %q: %w", p.Service, err)
		}
		return applied, nil
	}

	call := p.client.api.Namespaces.Services.ReplaceService(p.client.serviceName(p.Service), p.desired)
	if dryRun {
		call = call.DryRun(dryRunAll)
	}
	applied, err := call.Context(ctx).Do()
	if err != nil {
		if isConflict(err) {
			// resourceVersion is sent, so a 409 is the case where "someone rewrote it between
			// computing the diff and applying". The API's wording (version 'X' was specified but
			// current version is 'Y') alone does not say what to do, so rephrase it.
			return nil, fmt.Errorf("service %q changed after the diff was computed; re-run to compare against the current state: %w", p.Service, err)
		}
		return nil, fmt.Errorf("failed to update service %q: %w", p.Service, err)
	}
	return applied, nil
}

// setResourceVersion copies current's resourceVersion onto desired so that optimistic concurrency
// control takes effect. It does nothing when current is nil (a create).
func setResourceVersion(desired, current *run.Service) {
	if desired == nil || desired.Metadata == nil || current == nil || current.Metadata == nil {
		return
	}
	// If desired already carries a version, do not overwrite it. rollback / refresh / traffic read
	// live and then edit it, so desired carries the version *of that read*. Replacing it with the
	// newer version here would let the CAS wave through someone else's change made between the
	// two GETs and silently revert it (meaning to touch only traffic, the image from the deploy
	// just before would be rolled back too). Sent with the version it carries, that case becomes a
	// 409.
	if desired.Metadata.ResourceVersion != "" {
		return
	}
	desired.Metadata.ResourceVersion = current.Metadata.ResourceVersion
}

// DeleteService deletes the service. When dryRun is true, the server only validates.
// This cannot be undone, so the caller must obtain confirmation.
func (c *Client) DeleteService(ctx context.Context, service string, dryRun bool) error {
	call := c.api.Namespaces.Services.Delete(c.serviceName(service))
	if dryRun {
		call = call.DryRun(dryRunAll)
	}
	if _, err := call.Context(ctx).Do(); err != nil {
		return fmt.Errorf("failed to delete service %q: %w", service, err)
	}
	return nil
}

// AppliedGeneration extracts metadata.generation from Apply's return value nil-safely.
// If it cannot, it returns 0, in which case Wait looks only at Ready regardless of generation.
func AppliedGeneration(applied *run.Service) int64 {
	if applied == nil || applied.Metadata == nil {
		return 0
	}
	return applied.Metadata.Generation
}

// isNotFound reports whether err is a googleapi 404 error.
func isNotFound(err error) bool {
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		return gerr.Code == 404
	}
	return false
}

// retryableForbiddenReasons are the reasons for which even a 403 can recover by waiting. Google
// APIs sometimes return rate limiting and quota exhaustion as 403, and those clear with time. They
// have to be told apart from a 403 for missing permission, so they are distinguished by reason.
var retryableForbiddenReasons = map[string]bool{
	"rateLimitExceeded":       true,
	"userRateLimitExceeded":   true,
	"quotaExceeded":           true,
	"concurrentLimitExceeded": true,
}

// isRetryable reports whether an error fetching state during a wait can recover on retry.
//
// An error with no known status (dropped connection/DNS resolution failure/EOF, etc.) is treated
// as transient. The generated API client does not retry, so giving up the wait on a single 503
// would make deploy report a failure even though the apply succeeded. On the other hand, a 400 bad
// request or a 401/403 authentication/permission problem only gives the same result however long
// you wait, and although the cause is known from the first poll, it would fail only after holding
// CI until the timeout (10 minutes by default).
func isRetryable(err error) bool {
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) {
		return true
	}
	switch {
	case gerr.Code == http.StatusRequestTimeout, gerr.Code == http.StatusTooManyRequests:
		return true
	case gerr.Code == http.StatusForbidden:
		for _, item := range gerr.Errors {
			if retryableForbiddenReasons[item.Reason] {
				return true
			}
		}
		return false
	case gerr.Code >= 500:
		return true
	case gerr.Code >= 400:
		// 400/401/404/409 and the like, and a 403 that is not a rate limit (handled above). A
		// permanent failure.
		return false
	}
	return true
}

// isConflict reports whether err is a resourceVersion mismatch (an optimistic concurrency control
// failure). Cloud Run returns 409 for a stale (but well-formed) resourceVersion.
// A malformed one is a 400, so it does not end up here.
func isConflict(err error) bool {
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		return gerr.Code == 409
	}
	return false
}

// ToManifest removes the read-only fields added by the server and returns a Knative-style YAML
// manifest usable for deploying.
func ToManifest(obj *run.Service) ([]byte, error) {
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("failed to sanitize the manifest: %w", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("failed to sanitize the manifest: %w", err)
	}
	sanitizeMap(m)

	manifest, err := yaml.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("failed to convert the manifest to YAML: %w", err)
	}
	return manifest, nil
}

// compareServices is the comparison implementation shared by every path that computes a diff. It
// does not modify its arguments.
func compareServices(current, desired *run.Service, currentName, desiredName string) (string, error) {
	current = alignRevisionName(current, desired)

	desiredYAML, err := ToManifest(desired)
	if err != nil {
		return "", err
	}

	var currentYAML []byte
	if current != nil {
		if currentYAML, err = ToManifest(current); err != nil {
			return "", err
		}
	}
	return Diff(currentYAML, desiredYAML, currentName, desiredName)
}

// CheckSyntax only checks that the manifest parses strictly as a run.Service.
// It is used to surface local problems before anything that accesses the API (unlike Validate, it
// does not look at whether the service name matches or at required fields).
func CheckSyntax(manifest []byte) error {
	_, err := parseManifest(manifest)
	return err
}

// Validate checks that a local manifest is a valid Cloud Run service definition.
// It does not access the API and checks only the structure and the fields required to deploy. It
// returns nil when there are no problems, and a combined error when there are several.
func Validate(manifest []byte, service string) error {
	svc, err := parseManifest(manifest)
	if err != nil {
		return err
	}
	return validate(svc, service)
}

// parseManifest strictly parses a manifest into a run.Service. UnmarshalStrict also detects
// unknown fields (such as a misspelled field name).
func parseManifest(manifest []byte) (*run.Service, error) {
	var svc run.Service
	if err := yaml.UnmarshalStrict(manifest, &svc); err != nil {
		return nil, fmt.Errorf("failed to parse the manifest: %w", err)
	}
	return &svc, nil
}

// validate checks a parsed service definition.
func validate(svc *run.Service, service string) error {
	var errs []error
	if svc.ApiVersion != manifestAPIVersion {
		errs = append(errs, fmt.Errorf("apiVersion must be %q, got %q", manifestAPIVersion, svc.ApiVersion))
	}
	if svc.Kind != manifestKind {
		errs = append(errs, fmt.Errorf("kind must be %q, got %q", manifestKind, svc.Kind))
	}

	switch {
	case svc.Metadata == nil || svc.Metadata.Name == "":
		errs = append(errs, errors.New("metadata.name is required"))
	case svc.Metadata.Name != service:
		errs = append(errs, fmt.Errorf("metadata.name %q does not match service argument %q", svc.Metadata.Name, service))
	}

	containers := serviceContainers(svc)
	if len(containers) == 0 {
		errs = append(errs, errors.New("spec.template.spec.containers must define at least one container"))
	}
	for i, c := range containers {
		switch {
		case c == nil:
			errs = append(errs, fmt.Errorf("spec.template.spec.containers[%d] must not be null", i))
		case c.Image == "":
			errs = append(errs, fmt.Errorf("spec.template.spec.containers[%d].image is required", i))
		}
	}

	return errors.Join(errs...)
}

// templateSpec extracts a service definition's spec.template.spec (RevisionSpec) nil-safely.
// Shared by everything that looks under the template: containers, service account, volumes and
// so on.
func templateSpec(svc *run.Service) *run.RevisionSpec {
	if svc == nil || svc.Spec == nil || svc.Spec.Template == nil {
		return nil
	}
	return svc.Spec.Template.Spec
}

// templateMeta extracts a service definition's spec.template.metadata nil-safely.
func templateMeta(svc *run.Service) *run.ObjectMeta {
	if svc == nil || svc.Spec == nil || svc.Spec.Template == nil {
		return nil
	}
	return svc.Spec.Template.Metadata
}

// revisionName extracts spec.template.metadata.name (the revision name) nil-safely.
func revisionName(svc *run.Service) string {
	meta := templateMeta(svc)
	if meta == nil {
		return ""
	}
	return meta.Name
}

// RevisionName returns the revision name the manifest pins. The empty string if none is given.
func RevisionName(manifest []byte) (string, error) {
	svc, err := parseManifest(manifest)
	if err != nil {
		return "", err
	}
	return revisionName(svc), nil
}

// WithoutRevisionName returns the service with spec.template.metadata.name (the revision name)
// removed. It does not modify its argument; it shallow-copies only the chain that has to change
// (Spec / Template / Metadata).
//
// When the revision name is omitted, Cloud Run generates one on the server side, but when it is
// set explicitly, a revision with the same name and a different configuration cannot be created.
// Left as is in a manifest built from live, the second and later deploys that change the template
// would fail, so it is dropped from the manifest init scaffolds.
func WithoutRevisionName(svc *run.Service) *run.Service {
	if revisionName(svc) == "" {
		return svc
	}
	meta := *svc.Spec.Template.Metadata
	meta.Name = ""
	tmpl := *svc.Spec.Template
	tmpl.Metadata = &meta
	spec := *svc.Spec
	spec.Template = &tmpl
	out := *svc
	out.Spec = &spec
	return &out
}

// alignRevisionName, when desired does not specify a revision name, returns the current used for
// the comparison with its revision name dropped. It does not modify its arguments.
//
// A name Cloud Run generated is never returned on read; the field is present only when a client
// set it (gcloud run deploy --revision-suffix, Terraform's template.metadata.name, a manifest that
// pins one, or refresh). Unless the local side specifies one, treating such a live name the same as
// a server-managed field is correct. Otherwise a manifest that does not write a revision name would
// keep showing a diff that never goes away against a service one of those tools last wrote.
// When the local side sets one explicitly, it is kept on both sides and shown as a diff.
func alignRevisionName(current, desired *run.Service) *run.Service {
	if current == nil || revisionName(desired) != "" {
		return current
	}
	return WithoutRevisionName(current)
}

// serviceContainers extracts the list of containers from a service definition nil-safely.
func serviceContainers(svc *run.Service) []*run.Container {
	spec := templateSpec(svc)
	if spec == nil {
		return nil
	}
	return spec.Containers
}

// Diff returns the unified diff of current and desired. It returns the empty string when there
// are no differences.
func Diff(current, desired []byte, currentName, desiredName string) (string, error) {
	d := difflib.UnifiedDiff{
		A:        difflib.SplitLines(string(current)),
		B:        difflib.SplitLines(string(desired)),
		FromFile: currentName,
		ToFile:   desiredName,
		Context:  3,
	}
	out, err := difflib.GetUnifiedDiffString(d)
	if err != nil {
		return "", fmt.Errorf("failed to compute the diff: %w", err)
	}
	return out, nil
}

// sanitizeMap removes the read-only fields added by the server from the map.
func sanitizeMap(m map[string]interface{}) {
	// status is entirely server-side state information, so it is removed as a whole.
	delete(m, "status")

	// Remove the read-only fields and server-managed annotations of top-level metadata.
	if meta, ok := m["metadata"].(map[string]interface{}); ok {
		for _, k := range serverManagedMetaFields {
			delete(meta, k)
		}
		deleteMapKeys(meta, "annotations", serverManagedAnnotations)
		// Cloud Run puts cloud.googleapis.com/location on every service. Unless it is removed, a
		// hand-written manifest that does not include it keeps showing it as a removal diff
		// forever.
		deleteMapKeys(meta, "labels", serverManagedLabels)
	}

	// Remove the server-managed labels/annotations of spec.template.metadata.
	if spec, ok := m["spec"].(map[string]interface{}); ok {
		if tmpl, ok := spec["template"].(map[string]interface{}); ok {
			if tmeta, ok := tmpl["metadata"].(map[string]interface{}); ok {
				deleteMapKeys(tmeta, "annotations", serverManagedAnnotations)
				deleteMapKeys(tmeta, "labels", serverManagedLabels)
				// Do not output a metadata that has become empty. A local manifest normally has
				// no spec.template.metadata at all, so leaving an empty object would keep showing
				// "metadata: {}" as a diff that never goes away.
				if len(tmeta) == 0 {
					delete(tmpl, "metadata")
				}
			}
		}
	}
}

// deleteMapKeys removes the given keys from parent[field] (a map), and removes field itself once
// it is empty.
func deleteMapKeys(parent map[string]interface{}, field string, keys []string) {
	child, ok := parent[field].(map[string]interface{})
	if !ok {
		return
	}
	for _, k := range keys {
		delete(child, k)
	}
	if len(child) == 0 {
		delete(parent, field)
	}
}
