package cloudrun

import (
	"fmt"
	"regexp"
	"strings"

	run "google.golang.org/api/run/v1"
)

// artifactRegistryHost は Artifact Registry のホスト名。先頭がロケーション名になる
// (例: asia-northeast1-docker.pkg.dev, us-docker.pkg.dev)。
var artifactRegistryHost = regexp.MustCompile(`^([a-z0-9-]+)-docker\.pkg\.dev$`)

// defaultImageTag はタグもダイジェストも書かれていないときに参照されるタグ。
const defaultImageTag = "latest"

// imageRef はコンテナイメージの参照を分解したもの。Artifact Registry のイメージだけは
// 実在を確認できるので、その場合に必要な要素まで取り出す。
type imageRef struct {
	// Raw はマニフェストに書かれていた文字列。エラーメッセージに使う。
	Raw string
	// Host はレジストリのホスト名。省略時は Docker Hub とみなして空のまま。
	Host string
	// Location 以降は Artifact Registry のイメージのときだけ埋まる。
	Location string
	Project  string
	Repo     string
	// Path はリポジトリ以下のイメージパス。入れ子 ("team/app") もありうる。
	Path string
	// Tag と Digest は排他。どちらも無い場合は Tag が "latest" になる。
	Tag    string
	Digest string
}

// IsArtifactRegistry は参照が Artifact Registry のイメージかを返す。
func (r imageRef) IsArtifactRegistry() bool { return r.Location != "" }

// parseImageRef はイメージ参照を分解する。形式は [host/]path[:tag][@digest]。
// Artifact Registry でない参照も Host / Raw までは埋めて返す (確認できない理由を
// 説明するのに使う)。
func parseImageRef(image string) imageRef {
	ref := imageRef{Raw: image}
	rest := image

	// ダイジェストが先。タグの ":" と混同しないよう、先に切り離す。
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		ref.Digest = rest[i+1:]
		rest = rest[:i]
	}

	// 最初の要素がホスト名かどうか。"." か ":" を含むか localhost ならホスト。
	// これを取り違えると "team/app" のようなパスをホストとして扱ってしまう。
	if i := strings.Index(rest, "/"); i >= 0 {
		head := rest[:i]
		if strings.ContainsAny(head, ".:") || head == "localhost" {
			ref.Host = head
			rest = rest[i+1:]
		}
	}

	// 残りの末尾にタグが付きうる。"/" より後の ":" だけがタグ。
	if ref.Digest == "" {
		if i := strings.LastIndex(rest, ":"); i > strings.LastIndex(rest, "/") {
			ref.Tag = rest[i+1:]
			rest = rest[:i]
		} else {
			ref.Tag = defaultImageTag
		}
	}

	m := artifactRegistryHost.FindStringSubmatch(ref.Host)
	if m == nil {
		return ref
	}
	// Artifact Registry のパスは <project>/<repo>/<image...>。3 要素に満たなければ
	// イメージとして不完全なので、ロケーションを埋めず「確認できない」側に倒す。
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return ref
	}
	ref.Location, ref.Project, ref.Repo, ref.Path = m[1], parts[0], parts[1], parts[2]
	return ref
}

// resourceName は Artifact Registry API で実在を確認するリソース名を返す。
// ダイジェスト指定なら dockerImages、タグ指定なら packages/.../tags を引く。
//
// イメージパスの "/" は %2F にする。API が返す名前もこの形で、二重にエスケープすると
// 404 になる (実 API で確認済み) ので、ここでの置換以外のエスケープはしないこと。
func (r imageRef) resourceName() string {
	repo := fmt.Sprintf("projects/%s/locations/%s/repositories/%s", r.Project, r.Location, r.Repo)
	path := strings.ReplaceAll(r.Path, "/", "%2F")
	if r.Digest != "" {
		return fmt.Sprintf("%s/dockerImages/%s@%s", repo, path, r.Digest)
	}
	return fmt.Sprintf("%s/packages/%s/tags/%s", repo, path, r.Tag)
}

// containerImages はマニフェストが参照するイメージを重複なく順序どおりに集める。
func containerImages(svc *run.Service) []string {
	spec := templateSpec(svc)
	if spec == nil {
		return nil
	}
	seen := make(map[string]bool)
	var out []string
	for _, c := range spec.Containers {
		if c == nil || c.Image == "" || seen[c.Image] {
			continue
		}
		seen[c.Image] = true
		out = append(out, c.Image)
	}
	return out
}
