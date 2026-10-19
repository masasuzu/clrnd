package cloudrun

import (
	"errors"
	"fmt"
	"regexp"

	"google.golang.org/api/googleapi"
)

var (
	// regionPattern is the shape of a Google Cloud region: asia-northeast1, us-central1,
	// northamerica-northeast2.
	regionPattern = regexp.MustCompile(`^[a-z]+-[a-z]+[0-9]+$`)

	// projectPattern accepts a project ID (6 to 30 lowercase letters, digits and hyphens, starting
	// with a letter and not ending with a hyphen), the same with a legacy domain prefix
	// (example.com:my-project), and a project number, which the namespaces API takes as well.
	projectPattern = regexp.MustCompile(
		`^(?:(?:[a-z0-9][a-z0-9.-]*[a-z0-9]:)?[a-z][a-z0-9-]{4,28}[a-z0-9]|[0-9]+)$`)
)

// ValidateRegion checks that region has the shape of a region before anything is built from it.
//
// The region becomes part of the API endpoint's host name (regionalEndpoint), and the client sends
// the user's access token to that host. Unchecked, a config file a user merely cloned could set
// `region: "attacker.example/"` and have the token sent there; a slash, a '?' or a '#' is enough
// to move the host. The shape check is also what turns a typo into an error that names the region,
// rather than a request to a host that does not exist.
func ValidateRegion(region string) error {
	if !regionPattern.MatchString(region) {
		return fmt.Errorf("invalid region %q: want a Cloud Run region such as asia-northeast1", region)
	}
	return nil
}

// ValidateProject checks that project has the shape of a project ID or number. The project goes
// into every resource name clrnd sends, including the paths of the other APIs verify calls.
func ValidateProject(project string) error {
	if !projectPattern.MatchString(project) {
		return fmt.Errorf("invalid project %q: want a project ID such as my-project", project)
	}
	return nil
}

// validateTarget runs both checks, region first.
func validateTarget(project, region string) error {
	if err := ValidateRegion(region); err != nil {
		return err
	}
	return ValidateProject(project)
}

// EndpointNotFound reports whether err is a 404 that did not come from the Cloud Run API: an HTTP
// 404 whose body is not an API error (Google's front end answers a host it does not serve with a
// web page). A region of the right shape that Cloud Run does not have ends up here.
//
// It matters which 404 this is. The API's 404 means "no such resource", and the plan path treats
// it as "create it"; the front end's means the request never reached the API, and treating it
// the same would show every line of a manifest as an addition and plan a create.
func EndpointNotFound(err error) bool {
	var gerr *googleapi.Error
	return errors.As(err, &gerr) && gerr.Code == 404 && !isAPIError(gerr)
}

// isAPIError reports whether gerr was parsed from an API error body. googleapi only fills these
// fields from a JSON error; for any other body it keeps the raw body and nothing else.
func isAPIError(gerr *googleapi.Error) bool {
	return gerr.Message != "" || len(gerr.Errors) > 0 || len(gerr.Details) > 0
}
