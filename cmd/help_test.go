package cmd

import (
	"strings"
	"testing"
)

// TestRefreshHelpSaysWhenTheRevisionNameGoesAway は、refresh --help がリビジョン名の
// 消える条件を正確に説明していることを確認する。「次の deploy で消える」と無条件に
// 書くと実際の挙動と食い違う: 差分が無ければ applyPlan は何も適用せず、名前は残る。
// README は正しく限定しているので、ヘルプだけがずれると両者が矛盾する。
func TestRefreshHelpSaysWhenTheRevisionNameGoesAway(t *testing.T) {
	stdout, stderr, err := executeRoot(t, "refresh", "--help")
	if err != nil {
		t.Fatalf("refresh --help error = %v", err)
	}
	// cobra のヘルプは SetOut 側に出るが、取り違えても落ちないよう両方を見る。
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
