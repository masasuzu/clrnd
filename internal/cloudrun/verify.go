package cloudrun

import (
	"context"
	"fmt"
	"strings"

	artifactregistry "google.golang.org/api/artifactregistry/v1"
	iam "google.golang.org/api/iam/v1"
	"google.golang.org/api/option"
	run "google.golang.org/api/run/v1"
	secretmanager "google.golang.org/api/secretmanager/v1"
	sqladmin "google.golang.org/api/sqladmin/v1"
	vpcaccess "google.golang.org/api/vpcaccess/v1"
)

// RemoteCheck is the result of the remote existence checks. Missing describes resources confirmed
// not to exist (fails verify). Unchecked describes what could not be confirmed because of missing
// permissions, an unreachable API, no credentials and so on (does not fail verify; treated as a
// warning). The two are kept apart because failing on the latter would break the offline lint of
// a CI job that merely has an ambient project/region.
type RemoteCheck struct {
	Missing   []string
	Unchecked []string
}

// VerifyRemote checks through the API that the resources the manifest references exist. It
// complements Validate (local schema validation), confirming with ADC the existence of the
// service account, Secret Manager secrets and their versions, the VPC connector, Cloud SQL
// instances and container images. Only a 404 (does not exist) is returned as Missing; every other
// error (client initialization failure, missing permission, API disabled, etc.) goes to Unchecked.
//
// region is used only to expand a short VPC connector name into a full resource name. A connector
// is a regional resource and cannot be looked up by name alone. The image location is in the
// reference (host name) itself, the Cloud SQL project is in the connection name, and neither IAM
// nor Secret Manager takes a region, so there is no other use for it.
//
// opts, as with NewClient, is the extension point tests use to inject a fake API.
func VerifyRemote(ctx context.Context, project, region string, manifest []byte,
	opts ...option.ClientOption) (*RemoteCheck, error) {
	svc, err := parseManifest(manifest)
	if err != nil {
		return nil, err
	}

	sa := serviceAccountName(svc)
	secrets := secretNames(svc)
	res := &RemoteCheck{}

	if sa != "" {
		iamSvc, err := iam.NewService(ctx, opts...)
		if err != nil {
			res.Unchecked = append(res.Unchecked, fmt.Sprintf("service account %q: %v", sa, err))
		} else {
			// The project part is a wildcard. Cloud Run can run as a service account from another
			// project, so pinning it to the project being verified would make a valid setup a
			// 404 = Missing and fail verify.
			name := fmt.Sprintf("projects/-/serviceAccounts/%s", sa)
			if _, err := iamSvc.Projects.ServiceAccounts.Get(name).Context(ctx).Do(); err != nil {
				if isNotFound(err) {
					res.Missing = append(res.Missing, fmt.Sprintf("service account %q does not exist", sa))
				} else {
					res.Unchecked = append(res.Unchecked, fmt.Sprintf("service account %q: %v", sa, err))
				}
			}
		}
	}

	checkSecrets(ctx, res, svc, secrets, project, opts...)
	checkVPCConnector(ctx, res, svc, project, region, opts...)
	checkCloudSQL(ctx, res, svc, opts...)
	checkImages(ctx, res, containerImages(svc), opts...)

	return res, nil
}

// vpcConnectorAnnotation / cloudSQLAnnotation are annotations through which the manifest
// references resources outside Cloud Run. Each is the kind of reference that "fails only once
// deployed", so its existence is checked in the same way as the service account and secrets.
const (
	vpcConnectorAnnotation = "run.googleapis.com/vpc-access-connector"
	cloudSQLAnnotation     = "run.googleapis.com/cloudsql-instances"
)

// checkVPCConnector checks that the VPC connector exists.
//
// The annotation value can be either a short name (just the connector name) or a full resource
// name. A short name is completed with the deploy target's project and region: a connector is a
// regional resource, so this is the one place region is needed.
func checkVPCConnector(ctx context.Context, res *RemoteCheck, svc *run.Service,
	project, region string, opts ...option.ClientOption) {
	connector := templateAnnotation(svc, vpcConnectorAnnotation)
	if connector == "" {
		return
	}
	name := connector
	if !strings.HasPrefix(name, "projects/") {
		if region == "" {
			res.Unchecked = append(res.Unchecked,
				fmt.Sprintf("VPC connector %q: no region to resolve the short name against", connector))
			return
		}
		name = fmt.Sprintf("projects/%s/locations/%s/connectors/%s", project, region, connector)
	}

	svcAPI, err := vpcaccess.NewService(ctx, opts...)
	if err != nil {
		res.Unchecked = append(res.Unchecked, fmt.Sprintf("VPC connector %q: %v", connector, err))
		return
	}
	if _, err := svcAPI.Projects.Locations.Connectors.Get(name).Context(ctx).Do(); err != nil {
		if isNotFound(err) {
			res.Missing = append(res.Missing, fmt.Sprintf("VPC connector %q does not exist", connector))
			return
		}
		res.Unchecked = append(res.Unchecked, fmt.Sprintf("VPC connector %q: %v", connector, err))
	}
}

// checkCloudSQL checks that the Cloud SQL instances connected to exist. The value is a
// comma-separated list of "<project>:<region>:<instance>". The project is taken from the
// connection name, so an instance in another project is not wrongly reported as Missing.
func checkCloudSQL(ctx context.Context, res *RemoteCheck, svc *run.Service, opts ...option.ClientOption) {
	raw := templateAnnotation(svc, cloudSQLAnnotation)
	if raw == "" {
		return
	}

	var sqlSvc *sqladmin.Service
	for _, entry := range strings.Split(raw, ",") {
		conn := strings.TrimSpace(entry)
		if conn == "" {
			continue
		}
		// A value in the wrong shape is "could not confirm", not "does not exist". The format
		// Cloud Run accepts is fixed, but speaking up is preferred over misjudging it and failing
		// verify.
		//
		// It is split from the right because a domain-scoped project (example.com:my-project)
		// itself contains ":". Splitting into 3 from the left would turn a valid connection name
		// into a "wrong shape" warning every time.
		project, instance, ok := splitConnectionName(conn)
		if !ok {
			res.Unchecked = append(res.Unchecked,
				fmt.Sprintf("Cloud SQL instance %q: not in <project>:<region>:<instance> form", conn))
			continue
		}
		if sqlSvc == nil {
			created, err := sqladmin.NewService(ctx, opts...)
			if err != nil {
				res.Unchecked = append(res.Unchecked, fmt.Sprintf("Cloud SQL instance %q: %v", conn, err))
				continue
			}
			sqlSvc = created
		}
		if _, err := sqlSvc.Instances.Get(project, instance).Context(ctx).Do(); err != nil {
			if isNotFound(err) {
				res.Missing = append(res.Missing, fmt.Sprintf("Cloud SQL instance %q does not exist", conn))
				continue
			}
			res.Unchecked = append(res.Unchecked, fmt.Sprintf("Cloud SQL instance %q: %v", conn, err))
		}
	}
}

// splitConnectionName splits <project>:<region>:<instance> into the project and the instance.
// So that a domain-scoped project (example.com:my-project:<region>:<instance>) is handled too, it
// cuts off the two rightmost parts as region / instance and treats the rest as the project.
func splitConnectionName(conn string) (project, instance string, ok bool) {
	parts := strings.Split(conn, ":")
	if len(parts) < 3 {
		return "", "", false
	}
	instance = parts[len(parts)-1]
	region := parts[len(parts)-2]
	project = strings.Join(parts[:len(parts)-2], ":")
	if project == "" || region == "" || instance == "" {
		return "", "", false
	}
	return project, instance, true
}

// checkSecrets checks that the secrets exist, and that the *versions* they reference exist.
//
// Versions are checked separately because a deleted version of an existing secret (or a mistyped
// number) fails only once deployed. "latest" resolves as is too.
//
// When the secret itself was not found, its versions are not queried. Listing "secret X does not
// exist" alongside "secret X has no version latest" tells you nothing more; it only buries the
// real cause.
func checkSecrets(ctx context.Context, res *RemoteCheck, svc *run.Service, secrets []string,
	project string, opts ...option.ClientOption) {
	if len(secrets) == 0 {
		return
	}
	aliases := secretAliases(svc)
	versions := versionsBySecret(svc)

	smSvc, err := secretmanager.NewService(ctx, opts...)
	if err != nil {
		for _, s := range secrets {
			res.Unchecked = append(res.Unchecked, fmt.Sprintf("secret %q: %v", s, err))
		}
		return
	}

	for _, s := range secrets {
		name := secretResourceName(project, s, aliases)
		if _, err := smSvc.Projects.Secrets.Get(name).Context(ctx).Do(); err != nil {
			if isNotFound(err) {
				res.Missing = append(res.Missing, fmt.Sprintf("secret %q does not exist", s))
			} else {
				res.Unchecked = append(res.Unchecked, fmt.Sprintf("secret %q: %v", s, err))
			}
			continue
		}
		for _, version := range versions[s] {
			versionName := fmt.Sprintf("%s/versions/%s", name, version)
			got, err := smSvc.Projects.Secrets.Versions.Get(versionName).Context(ctx).Do()
			if err != nil {
				if isNotFound(err) {
					res.Missing = append(res.Missing,
						fmt.Sprintf("secret %q has no version %q", s, version))
					continue
				}
				res.Unchecked = append(res.Unchecked,
					fmt.Sprintf("secret %q version %q: %v", s, version, err))
				continue
			}
			// get returns 200 for destroyed and disabled versions too (it is access that stops
			// working). Without looking at the state, "a reference to a gone version passes
			// straight through", and adding this check would have been pointless.
			if state := got.State; state != "" && state != secretVersionEnabled {
				res.Missing = append(res.Missing,
					fmt.Sprintf("secret %q version %q is %s, so it cannot be read", s, version, state))
			}
		}
	}
}

// versionsBySecret collects, per secret, the referenced versions without duplicates.
func versionsBySecret(svc *run.Service) map[string][]string {
	out := make(map[string][]string)
	seen := make(map[secretVersionRef]bool)
	for _, ref := range secretVersionRefs(svc) {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		out[ref.Secret] = append(out[ref.Secret], ref.Version)
	}
	return out
}

// secretVersionEnabled is the state of a version that can be read. Any other state (DISABLED /
// DESTROYED) cannot be referenced, so it is treated the same as a version that does not exist.
const secretVersionEnabled = "ENABLED"

// secretVersionRef is a pair of a secret and one of its versions.
type secretVersionRef struct {
	Secret  string
	Version string
}

// secretVersionRefs collects the (secret, version) pairs the manifest references, without
// duplicates. The version is the secretKeyRef.key of an env entry and the items[].key of a secret
// volume. One with no version given is treated as "latest", as Cloud Run does.
func secretVersionRefs(svc *run.Service) []secretVersionRef {
	spec := templateSpec(svc)
	if spec == nil {
		return nil
	}

	seen := make(map[secretVersionRef]bool)
	var out []secretVersionRef
	add := func(secret, version string) {
		if secret == "" {
			return
		}
		// The version normally goes in key, but when name has the form
		// projects/<p>/secrets/<s>/versions/<v> it is embedded there
		// (secretResourceName handles this form explicitly). If there is no key, use that one.
		if version == "" {
			version = versionFromSecretPath(secret)
		}
		if version == "" {
			version = "latest"
		}
		ref := secretVersionRef{Secret: secret, Version: version}
		if !seen[ref] {
			seen[ref] = true
			out = append(out, ref)
		}
	}

	for _, c := range spec.Containers {
		if c == nil {
			continue
		}
		for _, e := range c.Env {
			if e != nil && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
				add(e.ValueFrom.SecretKeyRef.Name, e.ValueFrom.SecretKeyRef.Key)
			}
		}
	}
	for _, v := range spec.Volumes {
		if v == nil || v.Secret == nil {
			continue
		}
		if len(v.Secret.Items) == 0 {
			add(v.Secret.SecretName, "")
			continue
		}
		for _, item := range v.Secret.Items {
			if item != nil {
				add(v.Secret.SecretName, item.Key)
			}
		}
	}
	return out
}

// versionFromSecretPath returns the version from a name of the form
// projects/<p>/secrets/<s>/versions/<v>. The empty string if the name is not in that form.
func versionFromSecretPath(name string) string {
	const marker = "/versions/"
	if i := strings.Index(name, marker); i >= 0 {
		return name[i+len(marker):]
	}
	return ""
}

// templateAnnotation reads an annotation of spec.template.metadata nil-safely.
func templateAnnotation(svc *run.Service, key string) string {
	meta := templateMeta(svc)
	if meta == nil {
		return ""
	}
	return strings.TrimSpace(meta.Annotations[key])
}

// checkImages checks through Artifact Registry that containers[].image exists, and adds the
// results to res.
//
// Only Artifact Registry images can be checked. gcr.io has no equivalent API, and Docker Hub and
// the rest are out of scope from the start, so they are **skipped silently**. Putting them in
// Unchecked would print a warning on every run merely for using a Docker Hub image, and the
// warnings themselves would come to be skimmed over. Unchecked is reserved for "went to check and
// could not decide". What can be checked is written in the README and in verify's --help.
//
// Not falling back to "could not check = does not exist" is a requirement of this function (it
// would break the same way as #23). Behaviour confirmed against the real API:
//   - a missing repository / package / tag / digest is a 404, whichever one it is
//   - a project that does not exist (or cannot be accessed) is a 403, so it does not become Missing
//   - a public image (us-docker.pkg.dev/cloudrun/container/hello) can be read with ordinary ADC
func checkImages(ctx context.Context, res *RemoteCheck, images []string, opts ...option.ClientOption) {
	var refs []imageRef
	for _, img := range images {
		if ref := parseImageRef(img); ref.IsArtifactRegistry() {
			refs = append(refs, ref)
		}
	}
	if len(refs) == 0 {
		return
	}

	// The client is created only when there is something to check. A manifest whose images are
	// all on gcr.io should not be required to enable the Artifact Registry API.
	arSvc, err := artifactregistry.NewService(ctx, opts...)
	if err != nil {
		for _, ref := range refs {
			res.Unchecked = append(res.Unchecked, fmt.Sprintf("image %q: %v", ref.Raw, err))
		}
		return
	}
	for _, ref := range refs {
		name := ref.resourceName()
		var getErr error
		if ref.Digest != "" {
			_, getErr = arSvc.Projects.Locations.Repositories.DockerImages.Get(name).Context(ctx).Do()
		} else {
			_, getErr = arSvc.Projects.Locations.Repositories.Packages.Tags.Get(name).Context(ctx).Do()
		}
		switch {
		case getErr == nil:
		case isNotFound(getErr):
			res.Missing = append(res.Missing, fmt.Sprintf("image %q does not exist", ref.Raw))
		default:
			res.Unchecked = append(res.Unchecked, fmt.Sprintf("image %q: %v", ref.Raw, getErr))
		}
	}
}

// serviceAccountName extracts the manifest's runtime service account nil-safely.
func serviceAccountName(svc *run.Service) string {
	spec := templateSpec(svc)
	if spec == nil {
		return ""
	}
	return spec.ServiceAccountName
}

// secretNames collects, without duplicates, the Secret Manager secret names the manifest
// references. It looks at env secretKeyRef as well as secret volumes.
func secretNames(svc *run.Service) []string {
	spec := templateSpec(svc)
	if spec == nil {
		return nil
	}

	seen := make(map[string]bool)
	var out []string
	add := func(n string) {
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}

	for _, c := range spec.Containers {
		if c == nil {
			continue
		}
		for _, e := range c.Env {
			if e != nil && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
				add(e.ValueFrom.SecretKeyRef.Name)
			}
		}
	}
	for _, v := range spec.Volumes {
		if v != nil && v.Secret != nil {
			add(v.Secret.SecretName)
		}
	}
	return out
}

// secretAliasAnnotation is the annotation key holding alias definitions for references to secrets
// in another project. The value is a comma-separated list of "<alias>:projects/<p>/secrets/<s>".
const secretAliasAnnotation = "run.googleapis.com/secrets"

// secretAliases parses the run.googleapis.com/secrets annotation of spec.template.metadata and
// returns a map of alias name -> actual path (projects/<p>/secrets/<s>).
// For a secret in another project, secretKeyRef.name holds only the alias and the actual path is
// in this annotation, so without looking it up the existence check misjudges.
func secretAliases(svc *run.Service) map[string]string {
	if svc.Spec == nil || svc.Spec.Template == nil || svc.Spec.Template.Metadata == nil {
		return nil
	}
	raw := svc.Spec.Template.Metadata.Annotations[secretAliasAnnotation]
	if raw == "" {
		return nil
	}
	out := make(map[string]string)
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		// Split "<alias>:projects/<p>/secrets/<s>" at the first ":" (the actual path has no ":").
		if i := strings.Index(entry, ":"); i > 0 {
			out[entry[:i]] = entry[i+1:]
		}
	}
	return out
}

// secretResourceName turns a secret name into a Secret Manager resource name.
// A name already in projects/.../secrets/... form is kept as is (a trailing /versions/... is
// dropped). An alias for another project is resolved to the actual path through aliases. Anything
// else is taken to be a secret in the same project.
func secretResourceName(project, name string, aliases map[string]string) string {
	if strings.HasPrefix(name, "projects/") {
		if i := strings.Index(name, "/versions/"); i >= 0 {
			return name[:i]
		}
		return name
	}
	if path, ok := aliases[name]; ok {
		return secretResourceName(project, path, nil)
	}
	return fmt.Sprintf("projects/%s/secrets/%s", project, name)
}
