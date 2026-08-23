package cmd

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// readAll はリクエスト body を読む小さなヘルパ。
func readAll(r *http.Request) ([]byte, error) { return io.ReadAll(r.Body) }

// TestDiffOnAServiceThatDoesNotExistYet は、まだ作られていないサービスに対して
// diff が動くことを確認する。README が勧める「マニフェストを書く → diff →
// deploy」の初回で、以前はここが素の 404 で落ちていた。
func TestDiffOnAServiceThatDoesNotExistYet(t *testing.T) {
	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if echoDryRun(w, r) {
			return
		}
		if r.Method == http.MethodPost {
			// 未存在なので既定値の解決は Create の dry-run になる。
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
