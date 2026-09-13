package cmd

import "errors"

// ExitCodeDiff is the exit code meaning differences were found when --exit-code is given.
// It uses the same assignment as terraform plan -detailed-exitcode (0 = no differences,
// 1 = error, 2 = differences found). That way errors keep 1 and existing scripts do not
// break.
const ExitCodeDiff = 2

// errDiffFound is the sentinel returned when diff --exit-code finds differences.
var errDiffFound = errors.New("differences found")

// ExitCode returns the exit code for an error returned by Execute.
func ExitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errDiffFound):
		return ExitCodeDiff
	default:
		return 1
	}
}
