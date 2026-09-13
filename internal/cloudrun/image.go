package cloudrun

import (
	"fmt"
	"regexp"
	"strings"

	run "google.golang.org/api/run/v1"
)

// artifactRegistryHost matches an Artifact Registry host name. The leading part is the location
// name (e.g. asia-northeast1-docker.pkg.dev, us-docker.pkg.dev).
var artifactRegistryHost = regexp.MustCompile(`^([a-z0-9-]+)-docker\.pkg\.dev$`)

// defaultImageTag is the tag referenced when neither a tag nor a digest is written.
const defaultImageTag = "latest"

// imageRef is a container image reference split into its parts. Only Artifact Registry images
// can have their existence checked, so for those it also extracts the parts that check needs.
type imageRef struct {
	// Raw is the string as written in the manifest. Used in error messages.
	Raw string
	// Host is the registry host name. Left empty when omitted, which means Docker Hub.
	Host string
	// Location and the fields after it are filled only for an Artifact Registry image.
	Location string
	Project  string
	Repo     string
	// Path is the image path below the repository. It can be nested ("team/app").
	Path string
	// Tag and Digest are mutually exclusive. When neither is present, Tag is "latest".
	Tag    string
	Digest string
}

// IsArtifactRegistry reports whether the reference is an Artifact Registry image.
func (r imageRef) IsArtifactRegistry() bool { return r.Location != "" }

// parseImageRef splits an image reference. The form is [host/]path[:tag][@digest].
// A reference that is not Artifact Registry is still returned with Host / Raw filled (they are
// used to explain why it cannot be checked).
func parseImageRef(image string) imageRef {
	ref := imageRef{Raw: image}
	rest := image

	// The digest comes first. Cut it off up front so it is not confused with the ":" of a tag.
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		ref.Digest = rest[i+1:]
		rest = rest[:i]
	}

	// Whether the first element is a host name. It is a host if it contains "." or ":", or is
	// localhost. Getting this wrong treats a path such as "team/app" as a host.
	if i := strings.Index(rest, "/"); i >= 0 {
		head := rest[:i]
		if strings.ContainsAny(head, ".:") || head == "localhost" {
			ref.Host = head
			rest = rest[i+1:]
		}
	}

	// The end of what remains may carry a tag. Only a ":" after the last "/" is a tag.
	if ref.Digest == "" {
		if i := strings.LastIndex(rest, ":"); i > strings.LastIndex(rest, "/") {
			ref.Tag = rest[i+1:]
			rest = rest[:i]
		} else {
			ref.Tag = defaultImageTag
		}
	}

	m := artifactRegistryHost.FindStringSubmatch(ref.Host)
	if m == nil {
		return ref
	}
	// An Artifact Registry path is <project>/<repo>/<image...>. With fewer than 3 elements the
	// image is incomplete, so leave the location empty and fall back to "cannot be checked".
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return ref
	}
	ref.Location, ref.Project, ref.Repo, ref.Path = m[1], parts[0], parts[1], parts[2]
	return ref
}

// resourceName returns the resource name used to check existence with the Artifact Registry
// API. A digest looks up dockerImages; a tag looks up packages/.../tags.
//
// The "/" in the image path becomes %2F. The names the API returns are in this form too, and a
// double-escaped one is a 404 (confirmed against the real API), so do not escape anything
// beyond the replacement done here.
func (r imageRef) resourceName() string {
	repo := fmt.Sprintf("projects/%s/locations/%s/repositories/%s", r.Project, r.Location, r.Repo)
	path := strings.ReplaceAll(r.Path, "/", "%2F")
	if r.Digest != "" {
		return fmt.Sprintf("%s/dockerImages/%s@%s", repo, path, r.Digest)
	}
	return fmt.Sprintf("%s/packages/%s/tags/%s", repo, path, r.Tag)
}

// containerImages collects the images the manifest references, in order and without duplicates.
func containerImages(svc *run.Service) []string {
	spec := templateSpec(svc)
	if spec == nil {
		return nil
	}
	seen := make(map[string]bool)
	var out []string
	for _, c := range spec.Containers {
		if c == nil || c.Image == "" || seen[c.Image] {
			continue
		}
		seen[c.Image] = true
		out = append(out, c.Image)
	}
	return out
}
