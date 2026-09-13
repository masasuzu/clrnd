package cloudrun

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	run "google.golang.org/api/run/v1"
)

// Cloud Run's constraints on revision names, as confirmed from real API errors:
//   - "The revision name must be prefixed by the name of the enclosing Service with a trailing -"
//   - "only lowercase, digits, and hyphens; must begin with letter, and may not end with
//     hyphen; must be less than 64 characters."
const maxRevisionNameLen = 63

var revisionNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*[a-z0-9]$`)

// RefreshSuffix returns the revision name suffix refresh uses by default.
// It is a UTC timestamp down to the second so that a person can tell "when it was rolled out",
// and so that consecutive runs do not collide (a revision with the same name cannot be created
// again).
func RefreshSuffix(now time.Time) string {
	return "r" + now.UTC().Format("060102150405")
}

// RefreshTarget returns the live service's definition with a new revision name.
// It does not modify its argument; it shallow-copies only the Spec / Template / Metadata chain
// that has to change.
//
// clrnd does not manage revision names as a rule (init drops them, diff ignores them), but
// refresh is the exception. Cloud Run does not create a new revision unless spec.template
// changes, so "roll out again without changing the definition" can only be done by naming the
// revision explicitly. The name set here disappears with the next deploy that "carries a change"
// (from a manifest without a revision name). If the definition is identical, the diff is empty,
// nothing is applied, and the name stays.
func RefreshTarget(live *run.Service, service, suffix string) (*run.Service, error) {
	if live == nil || live.Spec == nil || live.Spec.Template == nil {
		return nil, errors.New("the live service has no spec.template to refresh")
	}
	if suffix == "" {
		return nil, errors.New("no revision suffix to apply")
	}

	name := service + "-" + suffix
	if err := validateRevisionName(name); err != nil {
		return nil, err
	}
	// The same name does not create a new revision. The diff would be empty and the command would
	// succeed with "No changes.", so nothing happens even though a rollout was intended.
	if revisionName(live) == name {
		return nil, fmt.Errorf(
			"revision %q is already the current template revision, so refreshing would do nothing; "+
				"wait a second or pass a different --revision-suffix", name)
	}
	// When traffic is pinned to specific revisions, a new revision is created but serves nothing.
	// This is the state right after a rollback. refresh cannot do its job there, so refuse.
	if !servesLatestRevision(live) {
		return nil, fmt.Errorf(
			"refresh would create a revision that receives no traffic: this service pins traffic to " +
				"specific revisions, so nothing follows the latest one; deploy the change you want, " +
				"or send traffic back to the latest revision first")
	}

	meta := run.ObjectMeta{}
	if live.Spec.Template.Metadata != nil {
		meta = *live.Spec.Template.Metadata
	}
	meta.Name = name

	template := *live.Spec.Template
	template.Metadata = &meta
	spec := *live.Spec
	spec.Template = &template
	out := *live
	out.Spec = &spec
	return &out, nil
}

// servesLatestRevision reports whether traffic goes to the latest revision.
// An unset spec.traffic is the same as Cloud Run's default (100% to latestRevision), so it is
// true. rollback pins traffic to a specific revision, so it is false after that.
func servesLatestRevision(live *run.Service) bool {
	if len(live.Spec.Traffic) == 0 {
		return true
	}
	for _, t := range live.Spec.Traffic {
		// A tag-only entry at 0% serves nothing, so it does not count.
		if t != nil && t.LatestRevision && t.Percent > 0 {
			return true
		}
	}
	return false
}

// validateRevisionName rejects locally the names Cloud Run would reject. Sending them to the
// server gives the same result, but this says what is wrong up front, in understandable terms.
func validateRevisionName(name string) error {
	if len(name) > maxRevisionNameLen {
		return fmt.Errorf(
			"revision name %q is %d characters; Cloud Run allows at most %d, so pass a shorter --revision-suffix",
			name, len(name), maxRevisionNameLen)
	}
	if !revisionNamePattern.MatchString(name) {
		return fmt.Errorf(
			"revision name %q is not valid; Cloud Run allows lowercase letters, digits and hyphens, "+
				"starting with a letter and not ending with a hyphen", name)
	}
	return nil
}
