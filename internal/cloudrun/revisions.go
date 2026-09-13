package cloudrun

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	run "google.golang.org/api/run/v1"
)

// serviceLabel is the label naming the service a revision belongs to. Used to filter the List.
const serviceLabel = "serving.knative.dev/service"

// listRevisionsPageLimit is how many items a single List fetches. The Continue token walks the
// whole list.
const listRevisionsPageLimit = 100

// listRevisionsMaxPages is the upper bound on paging. No real service has more than 100 * 1000
// revisions, so hitting this only happens when something is wrong, such as the server returning
// the same token over and over. Without a bound, items grows without limit, and a ctx with no
// --timeout has no way to stop it. When the bound is hit, it returns an error rather than the
// truncated list (see the ListRevisions comment for why).
const listRevisionsMaxPages = 1000

// Revision is a summary of a single revision belonging to a service. It is also the structure of
// the JSON output.
type Revision struct {
	Name string `json:"name"`
	// Image is the first container's image. It holds the same value as Images[0] and is kept for
	// JSON backward compatibility (so users who wrote jq '.[].image' are not silently broken).
	// New code should read Images.
	Image string `json:"image,omitempty"`
	// Images are the images of every container in this revision, in the order written in the
	// manifest; there are several when there is a sidecar.
	Images []string `json:"images,omitempty"`
	// Created is the creation time string the API returns (RFC3339).
	Created string `json:"created,omitempty"`
	// Ready is the Status of the Ready condition (True/False/Unknown). Empty if there is no
	// condition.
	Ready  string `json:"ready,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Percent is the total traffic currently going to this revision.
	Percent int64 `json:"percent"`
	// Tags are the traffic tags attached to this revision.
	Tags []string `json:"tags,omitempty"`
	// Pinned is whether spec.traffic names this revision. It is a marker that lets the revision be
	// protected even before a share shows up in status.traffic (during a rollout, or while some
	// entry is still unresolved). It is not display information, so it is not in the JSON.
	Pinned bool `json:"-"`
}

// IsReady reports whether the revision is Ready. An old revision that has lost its traffic stays
// Ready=True (Reason=Retired), so this decides "is this a usable version".
func (r Revision) IsReady() bool { return r.Ready == conditionTrue }

// Revisions is a list of revisions. It has its own type for display.
type Revisions []Revision

// ListRevisions returns the revisions belonging to a service, newest first.
// The traffic split exists only on the Service, so it fetches both and joins them.
//
// If paging does not end normally (the same Continue token comes back, or a token remains after
// the page limit is reached), it returns an error rather than the list read so far. Treating an
// incomplete list as complete can make rollback miss the current revision or the previous Ready
// revision and roll back to the wrong version. Even for revisions, which only displays, failing is
// better than silently missing entries.
func (c *Client) ListRevisions(ctx context.Context, service string) (Revisions, error) {
	svc, err := c.GetService(ctx, service)
	if err != nil {
		return nil, err
	}

	selector := fmt.Sprintf("%s=%s", serviceLabel, service)
	var items []*run.Revision
	token := ""
	for page := 0; ; page++ {
		if page >= listRevisionsMaxPages {
			return nil, fmt.Errorf("failed to list revisions of service %q: gave up after %d pages "+
				"with more still to read", service, listRevisionsMaxPages)
		}
		call := c.api.Namespaces.Revisions.List(c.parent()).
			LabelSelector(selector).
			Limit(listRevisionsPageLimit)
		if token != "" {
			call = call.Continue(token)
		}
		resp, err := call.Context(ctx).Do()
		if err != nil {
			return nil, fmt.Errorf("failed to list revisions of service %q: %w", service, err)
		}
		if resp.Metadata == nil || resp.Metadata.Continue == "" {
			items = append(items, resp.Items...)
			break
		}
		// If the same token comes back, the next page will be the same response. Following it
		// makes no progress, so stop — but the list so far "has read the same page twice" and
		// can hold both duplicates and gaps.
		if resp.Metadata.Continue == token {
			return nil, fmt.Errorf("failed to list revisions of service %q: pagination did not advance "+
				"(the API returned the same continue token twice)", service)
		}
		items = append(items, resp.Items...)
		token = resp.Metadata.Continue
	}

	return newRevisions(items, newStatus(svc), pinnedRevisionNames(svc)), nil
}

// pinnedRevisionNames collects the revisions spec.traffic names.
// The status side sometimes has no names until serving settles, so protection from deletion also
// looks at the declaration (spec).
func pinnedRevisionNames(svc *run.Service) map[string]bool {
	out := make(map[string]bool)
	if svc == nil || svc.Spec == nil {
		return out
	}
	followsLatest := false
	for _, t := range svc.Spec.Traffic {
		if t == nil {
			continue
		}
		if t.RevisionName != "" {
			out[t.RevisionName] = true
		}
		followsLatest = followsLatest || t.LatestRevision
	}

	// latestRevision: true writes no name, so the loop above protects nothing for it.
	// During a rollout the new revision can already be in the list while status.traffic shows no
	// share for it yet, so left as is, --keep 0 could delete "the version about to serve".
	// Fill in from status which revisions the declaration can resolve to.
	if followsLatest && svc.Status != nil {
		for _, name := range []string{
			svc.Status.LatestReadyRevisionName,   // the revision latestRevision points to now
			svc.Status.LatestCreatedRevisionName, // the revision it will point to once converged
		} {
			if name != "" {
				out[name] = true
			}
		}
	}
	return out
}

// newRevisions converts an API response into Revisions. It is pure, with no API access, so the
// formatting and ordering can be tested entirely through this function.
func newRevisions(items []*run.Revision, status *Status, pinned map[string]bool) Revisions {
	traffic := trafficByRevision(status)

	out := make(Revisions, 0, len(items))
	for _, item := range items {
		if item == nil {
			continue
		}
		r := Revision{Images: revisionImages(item)}
		if len(r.Images) > 0 {
			r.Image = r.Images[0]
		}
		if item.Metadata != nil {
			r.Name = item.Metadata.Name
			r.Created = item.Metadata.CreationTimestamp
		}
		if c := revisionReady(item); c != nil {
			r.Ready = c.Status
			r.Reason = c.Reason
		}
		if t, ok := traffic[r.Name]; ok {
			r.Percent = t.percent
			r.Tags = t.tags
		}
		r.Pinned = pinned[r.Name]
		out = append(out, r)
	}

	sortRevisionsNewestFirst(out)
	return out
}

// revisionTraffic is the total traffic going to one revision. The same revision can appear in
// more than one entry (one for a share, one for a tag), so they are combined.
type revisionTraffic struct {
	percent int64
	tags    []string
}

func trafficByRevision(s *Status) map[string]revisionTraffic {
	out := make(map[string]revisionTraffic)
	if s == nil {
		return out
	}
	for _, t := range s.Traffic {
		if t.RevisionName == "" {
			continue
		}
		current := out[t.RevisionName]
		current.percent += t.Percent
		if t.Tag != "" {
			current.tags = append(current.tags, t.Tag)
		}
		out[t.RevisionName] = current
	}
	return out
}

// revisionImages extracts a revision's container images nil-safely, in spec order.
// Cloud Run services can have sidecars, so returning only the first one would leave "an image
// that is running but not shown".
func revisionImages(r *run.Revision) []string {
	if r == nil || r.Spec == nil {
		return nil
	}
	var images []string
	for _, container := range r.Spec.Containers {
		if container != nil && container.Image != "" {
			images = append(images, container.Image)
		}
	}
	return images
}

// revisionReady extracts a revision's Ready condition nil-safely.
func revisionReady(r *run.Revision) *run.GoogleCloudRunV1Condition {
	if r == nil || r.Status == nil {
		return nil
	}
	for _, c := range r.Status.Conditions {
		if c != nil && c.Type == conditionReady {
			return c
		}
	}
	return nil
}

// sortRevisionsNewestFirst sorts by creation time, newest first. When the time cannot be parsed,
// it sorts by revision name in descending order (Cloud Run numbers revisions sequentially, so
// newer ones sort later).
func sortRevisionsNewestFirst(rs Revisions) {
	sort.SliceStable(rs, func(i, j int) bool {
		ti, oki := time.Parse(time.RFC3339, rs[i].Created)
		tj, okj := time.Parse(time.RFC3339, rs[j].Created)
		switch {
		case oki == nil && okj == nil && !ti.Equal(tj):
			return ti.After(tj)
		case (oki == nil) != (okj == nil):
			// Put the ones whose time could be parsed first.
			return oki == nil
		default:
			return rs[i].Name > rs[j].Name
		}
	})
}

// Text returns a human-readable table. It ends with a newline. Empty if there are no revisions.
func (rs Revisions) Text() string {
	if len(rs) == 0 {
		return ""
	}

	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "REVISION\tREADY\tTRAFFIC\tTAGS\tCREATED\tIMAGE")
	for _, r := range rs {
		fmt.Fprintf(w, "%s\t%s\t%d%%\t%s\t%s\t%s\n",
			dash(r.Name), dash(readyLabel(r)), r.Percent,
			dash(strings.Join(r.Tags, ",")), dash(r.Created), dash(strings.Join(r.Images, ",")))
	}
	// tabwriter only writes on Flush. Writing to a builder cannot fail.
	_ = w.Flush()
	return b.String()
}

// readyLabel builds what the READY column shows, with the reason appended when there is one.
func readyLabel(r Revision) string {
	if r.Ready == "" {
		return ""
	}
	if r.Reason == "" {
		return r.Ready
	}
	return fmt.Sprintf("%s (%s)", r.Ready, r.Reason)
}

// dash turns an empty cell into "-", so the columns do not shift and become hard to read.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// revisionName builds a revision's resource name for the namespaces API.
func (c *Client) revisionName(revision string) string {
	return fmt.Sprintf("namespaces/%s/revisions/%s", c.project, revision)
}

// DeleteRevision deletes a single revision. Cloud Run does not delete old revisions on its own, so
// cleaning up can only be done by calling this.
func (c *Client) DeleteRevision(ctx context.Context, revision string) error {
	if _, err := c.api.Namespaces.Revisions.Delete(c.revisionName(revision)).Context(ctx).Do(); err != nil {
		return fmt.Errorf("failed to delete revision %q: %w", revision, err)
	}
	return nil
}

// SelectPrunableRevisions chooses, from a newest-first list, the revisions that may be deleted.
// It assumes revisions is in the "newest first" order ListRevisions returns.
//
// The rules are as follows, and each exists so that nothing whose loss would hurt is deleted.
//   - The newest keep entries are left as they are (counted whether or not they are protected.
//     Counting any other way would make the result disagree with the number given: --keep 3
//     could leave 4, or as many extra old versions as there are protected revisions could be
//     deleted)
//   - Even when older than that, anything receiving traffic, named by spec.traffic, or carrying a
//     tag is kept. Deleting a serving version takes the service down, and a tag is the entry point
//     of a URL, so deleting it removes that route. spec is consulted too because the share on the
//     status side can be absent until a rollout settles, and in that window everything looks
//     like "0%"
//
// A negative keep must be rejected by the caller (clamping it to 0 here would let a CI job that
// miscomputed it delete everything that is not protected).
func SelectPrunableRevisions(revisions Revisions, keep int) Revisions {
	if keep < 0 {
		keep = 0
	}
	var out Revisions
	for i, r := range revisions {
		if i < keep {
			continue
		}
		if r.Percent > 0 || r.Pinned || len(r.Tags) > 0 {
			continue
		}
		out = append(out, r)
	}
	return out
}
