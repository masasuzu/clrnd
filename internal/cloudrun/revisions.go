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

// serviceLabel はリビジョンが属するサービスを示すラベル。List の絞り込みに使う。
const serviceLabel = "serving.knative.dev/service"

// listRevisionsPageLimit は 1 回の List で取る件数。Continue トークンで全件たどる。
const listRevisionsPageLimit = 100

// listRevisionsMaxPages はページ送りの上限。1 サービスあたり 100 * 1000 件を超える
// リビジョンは現実には存在しないので、ここに当たるのはサーバが同じトークンを返し続けた
// ような異常時に限る。上限が無いと items が際限なく伸び、--timeout の無い ctx では
// 止める手段が無くなる。上限に当たった場合は打ち切った一覧を返さずエラーにする
// (理由は ListRevisions のコメントを参照)。
const listRevisionsMaxPages = 1000

// Revision はサービスに属するリビジョン 1 件の要約。JSON 出力の構造でもある。
type Revision struct {
	Name string `json:"name"`
	// Image は最初のコンテナのイメージ。
	Image string `json:"image,omitempty"`
	// Created は API が返す作成時刻の文字列 (RFC3339)。
	Created string `json:"created,omitempty"`
	// Ready は Ready 条件の Status (True/False/Unknown)。条件が無ければ空。
	Ready  string `json:"ready,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Percent は現在このリビジョンに向いているトラフィックの合計。
	Percent int64 `json:"percent"`
	// Tags はこのリビジョンに付いたトラフィックタグ。
	Tags []string `json:"tags,omitempty"`
}

// IsReady はリビジョンが Ready かを返す。トラフィックを失った古いリビジョンも
// Ready=True (Reason=Retired) のままなので、これは「使える版か」の判定になる。
func (r Revision) IsReady() bool { return r.Ready == conditionTrue }

// Revisions はリビジョン一覧。表示のために型を付けている。
type Revisions []Revision

// ListRevisions はサービスに属するリビジョンを新しい順に返す。
// トラフィック配分は Service 側にしか無いので、両方を引いて突き合わせる。
//
// ページ送りが正常に終わらなかった場合 (同じ Continue トークンが返る、上限ページ数に
// 達してもトークンが残る) は、そこまでの一覧ではなくエラーを返す。不完全な一覧を
// 完全なものとして扱うと、rollback が現行のリビジョンや直前の Ready なリビジョンを
// 見落として誤った版へ戻しうる。表示だけの revisions でも、黙って欠けるより落ちたほうがよい。
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
		// 同じトークンが返ってきたら、次のページも同じ応答になる。追い続けても進まないので
		// 止めるが、ここまでの一覧は「同じページを 2 回読んだもの」で重複も欠落もありうる。
		if resp.Metadata.Continue == token {
			return nil, fmt.Errorf("failed to list revisions of service %q: pagination did not advance "+
				"(the API returned the same continue token twice)", service)
		}
		items = append(items, resp.Items...)
		token = resp.Metadata.Continue
	}

	return newRevisions(items, newStatus(svc)), nil
}

// newRevisions は API のレスポンスを Revisions に変換する。API アクセスを伴わない
// 純粋な処理なので、整形や並び順の検証はこの関数だけで完結できる。
func newRevisions(items []*run.Revision, status *Status) Revisions {
	traffic := trafficByRevision(status)

	out := make(Revisions, 0, len(items))
	for _, item := range items {
		if item == nil {
			continue
		}
		r := Revision{Image: revisionImage(item)}
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
		out = append(out, r)
	}

	sortRevisionsNewestFirst(out)
	return out
}

// revisionTraffic は 1 リビジョンに向いたトラフィックの合計。同じリビジョンが
// 割合用とタグ用で複数エントリに現れることがあるのでまとめる。
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

// revisionImage はリビジョンのコンテナイメージを nil セーフに取り出す。
func revisionImage(r *run.Revision) string {
	if r == nil || r.Spec == nil {
		return ""
	}
	for _, container := range r.Spec.Containers {
		if container != nil && container.Image != "" {
			return container.Image
		}
	}
	return ""
}

// revisionReady はリビジョンの Ready 条件を nil セーフに取り出す。
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

// sortRevisionsNewestFirst は作成時刻の新しい順に並べる。時刻が読めない場合は
// リビジョン名の降順にする (Cloud Run の採番は連番なので新しいものが後ろ)。
func sortRevisionsNewestFirst(rs Revisions) {
	sort.SliceStable(rs, func(i, j int) bool {
		ti, oki := time.Parse(time.RFC3339, rs[i].Created)
		tj, okj := time.Parse(time.RFC3339, rs[j].Created)
		switch {
		case oki == nil && okj == nil && !ti.Equal(tj):
			return ti.After(tj)
		case (oki == nil) != (okj == nil):
			// 時刻が読めたものを先に出す。
			return oki == nil
		default:
			return rs[i].Name > rs[j].Name
		}
	})
}

// Text は人間向けの表を返す。末尾は改行で終わる。空なら空文字列。
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
			dash(strings.Join(r.Tags, ",")), dash(r.Created), dash(r.Image))
	}
	// tabwriter は Flush で初めて書き出す。失敗はビルダー相手では起きない。
	_ = w.Flush()
	return b.String()
}

// readyLabel は READY 列の表示を組み立てる。理由があれば添える。
func readyLabel(r Revision) string {
	if r.Ready == "" {
		return ""
	}
	if r.Reason == "" {
		return r.Ready
	}
	return fmt.Sprintf("%s (%s)", r.Ready, r.Reason)
}

// dash は空欄を "-" にする。列がずれて読みにくくならないようにするため。
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
