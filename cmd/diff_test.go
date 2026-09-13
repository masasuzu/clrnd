package cmd

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// readAll is a small helper that reads the request body.
func readAll(r *http.Request) ([]byte, error) { return io.ReadAll(r.Body) }

// TestDiffOnAServiceThatDoesNotExistYet checks that diff works against a service that has not
// been created yet. This is the first run of the "write a manifest → diff → deploy" flow the
// README recommends, and it used to fail here with a bare 404.
func TestDiffOnAServiceThatDoesNotExistYet(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if echoDryRun(w, r) {
			return
		}
		if r.Method == http.MethodPost {
			// The service does not exist, so resolving the defaults is a dry-run Create.
			body, _ := readAll(r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error": {"code": 404, "message": "not found"}}`))
	})

	manifest := writeManifest(t, localManifest)
	stdout, _, err := executeRoot(t, "diff", "my-svc", manifest,
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("diff error = %v, want a service that does not exist yet to be handled", err)
	}
	if !strings.Contains(stdout, "+kind: Service") {
		t.Errorf("diff stdout = %q, want the whole manifest shown as an addition", stdout)
	}
}
