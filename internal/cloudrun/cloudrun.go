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

// serviceKind describes Cloud Run services.
var serviceKind = &kindOf[*run.Service]{
	name:       manifestKind,
	noun:       "service",
	apiVersion: manifestAPIVersion,

	newObject: func() *run.Service { return &run.Service{} },
	typeMeta:  func(s *run.Service) (string, string) { return s.ApiVersion, s.Kind },
	meta: func(s *run.Service) *run.ObjectMeta {
		if s == nil {
			return nil
		}
		return s.Metadata
	},
	workload: serviceWorkload,
	status:   newStatus,

	get: func(ctx context.Context, c *Client, name string) (*run.Service, error) {
		return c.api.Namespaces.Services.Get(c.serviceName(name)).Context(ctx).Do()
	},
	create: func(ctx context.Context, c *Client, obj *run.Service, dryRun bool) (*run.Service, error) {
		call := c.api.Namespaces.Services.Create(c.parent(), obj)
		if dryRun {
			call = call.DryRun(dryRunAll)
		}
		return call.Context(ctx).Do()
	},
	replace: func(ctx context.Context, c *Client, name string, obj *run.Service, dryRun bool) (*run.Service, error) {
		call := c.api.Namespaces.Services.ReplaceService(c.serviceName(name), obj)
		if dryRun {
			call = call.DryRun(dryRunAll)
		}
		return call.Context(ctx).Do()
	},
	del: func(ctx context.Context, c *Client, name string, dryRun bool) error {
		call := c.api.Namespaces.Services.Delete(c.serviceName(name))
		if dryRun {
			call = call.DryRun(dryRunAll)
		}
		_, err := call.Context(ctx).Do()
		return err
	},

	prepare: func(desired, current *run.Service, name string, opts PlanOptions) (*run.Service, error) {
		if !opts.KeepSplit {
			return desired, nil
		}
		return keepTraffic(desired, current, name)
	},
	template:  serviceTemplate,
	revisions: serviceRevisions,
	align:     alignRevisionName,
	// Keeping the live revision name makes the second deploy that changes the template fail with
	// "a revision with the same name and a different configuration cannot be created". The
	// scaffold drops it and leaves naming to Cloud Run's automatic numbering.
	scaffold: WithoutRevisionName,
}

// parseManifest parses a service manifest strictly (see kindOf.parse).
func parseManifest(manifest []byte) (*run.Service, error) {
	return serviceKind.parse(manifest)
}

// Read-only annotations added by the server. A manifest for deploying does not need them.
// Applied to top-level metadata and to spec.template.metadata.
//
// client-name / client-version record "the tool that last wrote", not configuration. Importing a
// service created with gcloud through init would bake "gcloud" into the manifest, and every later
// clrnd deploy would keep sending it back. With a hand-written manifest it shows up the other way
// round, as a removal diff that never goes away. clrnd does not manage these.
//
// The lists are shared by every kind. A job records who created and last changed it under
// run.googleapis.com/ rather than serving.knative.dev/; no service carries those keys, so removing
// them is harmless there.
//
// A key missing from these lists shows up as a diff that never goes away, but only under
// --no-server-defaults: with server defaults resolved, the dry run fills the key in on both sides
// and hides the omission. That is why the E2E asserts a clean diff on the --no-server-defaults
// path.
var serverManagedAnnotations = []string{
	"run.googleapis.com/operation-id",
	"run.googleapis.com/ingress-status",
	"run.googleapis.com/urls",
	"run.googleapis.com/client-name",
	"run.googleapis.com/client-version",
	"serving.knative.dev/creator",
	"serving.knative.dev/lastModifier",
	"run.googleapis.com/creator",
	"run.googleapis.com/lastModifier",
}

// Read-only labels added by the server. Applied to top-level metadata and to
// spec.template.metadata (cloud.googleapis.com/location is in practice only set on top-level
// metadata, but removing it from the template side does no harm, so the lists are not split).
//
// run.googleapis.com/lastUpdatedTime is set on a job and is a timestamp: left in, it would differ
// between the live job and the dry run that resolves defaults, so every diff would show it.
var serverManagedLabels = []string{
	"client.knative.dev/nonce",
	"run.googleapis.com/startupProbeType",
	"cloud.googleapis.com/location",
	"run.googleapis.com/lastUpdatedTime",
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

// isNotFound reports whether err is a googleapi 404 error.
func isNotFound(err error) bool {
	var gerr *googleapi.Error
	// Only the API's own 404 says the resource does not exist; see EndpointNotFound.
	return errors.As(err, &gerr) && gerr.Code == 404 && isAPIError(gerr)
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
	return toManifest(obj)
}

// toManifest is ToManifest for any kind's API type.
func toManifest(obj any) ([]byte, error) {
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

// serviceTemplate returns a service's spec.template nil-safely.
func serviceTemplate(svc *run.Service) *run.RevisionTemplate {
	if svc == nil || svc.Spec == nil {
		return nil
	}
	return svc.Spec.Template
}

// withServiceTemplate returns a copy of svc holding t as its spec.template. It does not modify svc:
// only the service and its spec are copied.
func withServiceTemplate(svc *run.Service, t *run.RevisionTemplate) *run.Service {
	out := *svc
	spec := run.ServiceSpec{}
	if svc.Spec != nil {
		spec = *svc.Spec
	}
	spec.Template = t
	out.Spec = &spec
	return &out
}

// revisionName extracts a service's spec.template.metadata.name (the revision name) nil-safely.
func revisionName(svc *run.Service) string {
	return revisionNameOf(serviceTemplate(svc))
}

// RevisionName returns the revision name a manifest of the given kind pins: "" when it pins none,
// and always "" for a kind without revisions (a job).
func RevisionName(kind Kind, manifest []byte) (string, error) {
	return kind.revisionName(manifest)
}

// WithoutRevisionName returns the service with spec.template.metadata.name (the revision name)
// removed. It does not modify its argument; it shallow-copies only the chain that has to change
// (Spec / Template / Metadata).
//
// When the revision name is omitted, Cloud Run generates one on the server side, but when it is
// set explicitly, a revision with the same name and a different configuration cannot be created.
// Left as is in a manifest built from live, the second and later deploys that change the template
// would fail, so it is dropped from the manifest init scaffolds.
var WithoutRevisionName = withoutRevisionNameOf(serviceTemplate, withServiceTemplate)

// alignRevisionName, when desired does not specify a revision name, returns the current used for
// the comparison with its revision name dropped. It does not modify its arguments.
//
// A name Cloud Run generated is never returned on read; the field is present only when a client
// set it (gcloud run deploy --revision-suffix, Terraform's template.metadata.name, a manifest that
// pins one, or refresh). Unless the local side specifies one, treating such a live name the same as
// a server-managed field is correct. Otherwise a manifest that does not write a revision name would
// keep showing a diff that never goes away against a service one of those tools last wrote.
// When the local side sets one explicitly, it is kept on both sides and shown as a diff.
var alignRevisionName = alignRevisionNameOf(serviceTemplate, withServiceTemplate)

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
