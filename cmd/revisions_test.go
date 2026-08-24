package cmd

import (
	"net/http"
	"strings"
	"sync"
	"testing"
)

// startPruneAPI はサービスとリビジョン一覧に応え、DELETE されたリビジョン名を記録する。
func startPruneAPI(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var deleted []string

	startFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			parts := strings.Split(r.URL.Path, "/")
			mu.Lock()
			deleted = append(deleted, parts[len(parts)-1])
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/revisions") {
			_, _ = w.Write([]byte(revisionsJSON))
			return
		}
		_, _ = w.Write([]byte(rollbackServiceJSON))
	})

	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), deleted...)
	}
}

// TestRevisionsPruneDeletesTheOldOnes は、--keep より古いリビジョンが消えることを
// 確認する。Cloud Run は古い版を自動では消さないので、これが唯一の掃除手段。
func TestRevisionsPruneDeletesTheOldOnes(t *testing.T) {
	deleted := startPruneAPI(t)

	stdout, _, err := executeRoot(t, "revisions", "my-svc", "--prune", "--keep", "1",
		"--auto-approve", "--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("revisions --prune error = %v", err)
	}
	if got := deleted(); len(got) != 1 || got[0] != "my-svc-00006-def" {
		t.Errorf("deleted = %v, want only the older revision", got)
	}
	// 消す対象は stdout にデータとして出す。
	if !strings.Contains(stdout, "my-svc-00006-def") {
		t.Errorf("stdout = %q, want the revisions to be listed before deleting", stdout)
	}
}

// TestRevisionsPruneKeepsTheServingRevision は、配信中のリビジョンが --keep 0 でも
// 残ることを確認する。ここを消すとサービスが落ちる。
func TestRevisionsPruneKeepsTheServingRevision(t *testing.T) {
	deleted := startPruneAPI(t)

	if _, _, err := executeRoot(t, "revisions", "my-svc", "--prune", "--keep", "0",
		"--auto-approve", "--project", "test-project", "--region", "asia-northeast1"); err != nil {
		t.Fatalf("revisions --prune error = %v", err)
	}
	for _, name := range deleted() {
		if name == "my-svc-00007-abc" {
			t.Errorf("deleted = %v, want the revision serving traffic to be kept", deleted())
		}
	}
}

// TestRevisionsPruneDryRunDeletesNothing は、--dry-run が一覧だけ出して何も消さない
// ことを確認する。確認プロンプトも出さない (delete と同じ方針)。
func TestRevisionsPruneDryRunDeletesNothing(t *testing.T) {
	deleted := startPruneAPI(t)

	stdout, stderr, err := executeRoot(t, "revisions", "my-svc", "--prune", "--keep", "0", "--dry-run",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("revisions --prune --dry-run error = %v", err)
	}
	if got := deleted(); len(got) != 0 {
		t.Errorf("deleted = %v, want nothing deleted on a dry run", got)
	}
	if !strings.Contains(stdout, "my-svc-00006-def") {
		t.Errorf("stdout = %q, want the candidates listed", stdout)
	}
	if !strings.Contains(stderr, "Dry run") {
		t.Errorf("stderr = %q, want the dry run reported", stderr)
	}
}

// TestRevisionsPruneRefusesWithoutConfirmation は、非対話環境で --auto-approve が
// 無ければ何も消さないことを確認する。破壊的な操作は delete と同じ扱いにする。
func TestRevisionsPruneRefusesWithoutConfirmation(t *testing.T) {
	deleted := startPruneAPI(t)

	_, _, err := executeRoot(t, "revisions", "my-svc", "--prune", "--keep", "0",
		"--project", "test-project", "--region", "asia-northeast1")
	if err == nil {
		t.Fatal("revisions --prune error = nil, want a refusal without a terminal")
	}
	if !strings.Contains(err.Error(), "refusing to prune without confirmation") {
		t.Errorf("error = %v", err)
	}
	if got := deleted(); len(got) != 0 {
		t.Errorf("deleted = %v, want nothing deleted", got)
	}
}

// TestRevisionsWithoutPruneStaysReadOnly は、--prune を渡さない限り一覧表示のままで
// あることを確認する。
func TestRevisionsWithoutPruneStaysReadOnly(t *testing.T) {
	deleted := startPruneAPI(t)

	if _, _, err := executeRoot(t, "revisions", "my-svc",
		"--project", "test-project", "--region", "asia-northeast1"); err != nil {
		t.Fatalf("revisions error = %v", err)
	}
	if got := deleted(); len(got) != 0 {
		t.Errorf("deleted = %v, want listing to delete nothing", got)
	}
}

// TestRevisionsPruneFlagsNeedPrune は、--prune 無しで掃除用のフラグを受け取ったときに
// 黙って無視しないことを確認する。無視すると「掃除したつもりで一覧を見ただけ」の実行が
// 成功して終わる。
func TestRevisionsPruneFlagsNeedPrune(t *testing.T) {
	deleted := startPruneAPI(t)

	for _, flag := range [][]string{{"--keep", "5"}, {"--auto-approve"}, {"--dry-run"}} {
		args := append([]string{"revisions", "my-svc"}, flag...)
		args = append(args, "--project", "test-project", "--region", "asia-northeast1")
		_, _, err := executeRoot(t, args...)
		if err == nil || !strings.Contains(err.Error(), "only applies with --prune") {
			t.Errorf("revisions %v error = %v, want it to reject the flag without --prune", flag, err)
		}
	}
	if got := deleted(); len(got) != 0 {
		t.Errorf("deleted = %v, want nothing deleted", got)
	}
}

// TestRevisionsPruneRejectsANegativeKeep は、負の保持数を 0 に丸めないことを確認する。
// 丸めると、計算を誤った CI が保護対象以外を全部消してしまう。
func TestRevisionsPruneRejectsANegativeKeep(t *testing.T) {
	deleted := startPruneAPI(t)

	_, _, err := executeRoot(t, "revisions", "my-svc", "--prune", "--keep", "-1", "--auto-approve",
		"--project", "test-project", "--region", "asia-northeast1")
	if err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("revisions --keep -1 error = %v, want it rejected", err)
	}
	if got := deleted(); len(got) != 0 {
		t.Errorf("deleted = %v, want nothing deleted", got)
	}
}

// TestRevisionsPruneJSONWithNothingToDo は、対象が無い日でも JSON が空にならないことを
// 確認する。`| jq 'length'` のような使い方が、その日だけ壊れないように。
func TestRevisionsPruneJSONWithNothingToDo(t *testing.T) {
	startPruneAPI(t)

	stdout, stderr, err := executeRoot(t, "revisions", "my-svc", "--prune", "--keep", "100",
		"--dry-run", "--format", "json",
		"--project", "test-project", "--region", "asia-northeast1")
	if err != nil {
		t.Fatalf("revisions --prune error = %v", err)
	}
	if strings.TrimSpace(stdout) != "[]" {
		t.Errorf("stdout = %q, want an empty JSON array", stdout)
	}
	if !strings.Contains(stderr, "Nothing to prune") {
		t.Errorf("stderr = %q, want the status line", stderr)
	}
}
