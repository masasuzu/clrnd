package cmd

import (
	"strings"
	"testing"
)

// TestRefreshHelpSaysWhenTheRevisionNameGoesAway checks that refresh --help accurately describes
// when the revision name goes away. Saying unconditionally "the next deploy removes it" disagrees
// with the actual behaviour: when there is no difference, applyPlan applies nothing and the name
// stays. The README qualifies this correctly, so if only the help drifts, the two contradict each
// other.
func TestRefreshHelpSaysWhenTheRevisionNameGoesAway(t *testing.T) {
	stdout, stderr, err := executeRoot(t, "refresh", "--help")
	if err != nil {
		t.Fatalf("refresh --help error = %v", err)
	}
	// cobra writes help to the SetOut side, but look at both streams so the test does not fail
	// if they get mixed up.
	help := stdout + stderr
	for _, want := range []string{
		"actually changes something drops that name again",
		"with no difference applies nothing",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("refresh --help does not mention %q:\n%s", want, help)
		}
	}
}
