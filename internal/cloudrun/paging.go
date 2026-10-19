package cloudrun

import (
	"fmt"

	run "google.golang.org/api/run/v1"
)

// listPageLimit is how many items a single List fetches. The Continue token walks the whole list.
const listPageLimit = 100

// listMaxPages is the upper bound on paging. Nothing clrnd lists — a service's revisions, a job's
// executions — comes near 100 * 1000 items, so hitting this only happens when something is wrong,
// such as the server returning the same token over and over. Without a bound, the items grow
// without limit, and a ctx with no --timeout has no way to stop it.
const listMaxPages = 1000

// listAll pages through a List call. fetch gets the continue token ("" for the first page) and
// returns that page's items and the token for the next one ("" when it was the last). what names
// the list in errors ("revisions of service \"x\"").
//
// If paging does not end normally (the same token comes back, or a token remains after
// listMaxPages), it returns an error rather than the items read so far. A list read that way is
// not just short: following a repeated token reads the same page twice, so it can hold duplicates
// as well as gaps, and a caller that treats it as complete acts on the wrong set.
func listAll[T any](what string, fetch func(token string) ([]T, string, error)) ([]T, error) {
	var items []T
	token := ""
	for page := 0; ; page++ {
		if page >= listMaxPages {
			return nil, fmt.Errorf("failed to list %s: gave up after %d pages with more still to read",
				what, listMaxPages)
		}
		got, next, err := fetch(token)
		if err != nil {
			return nil, fmt.Errorf("failed to list %s: %w", what, err)
		}
		items = append(items, got...)
		if next == "" {
			return items, nil
		}
		if next == token {
			return nil, fmt.Errorf("failed to list %s: pagination did not advance "+
				"(the API returned the same continue token twice)", what)
		}
		token = next
	}
}

// continueToken extracts a List response's continue token nil-safely.
func continueToken(meta *run.ListMeta) string {
	if meta == nil {
		return ""
	}
	return meta.Continue
}
