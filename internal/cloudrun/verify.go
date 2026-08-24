package cloudrun

import (
	"context"
	"fmt"
	"strings"

	artifactregistry "google.golang.org/api/artifactregistry/v1"
	iam "google.golang.org/api/iam/v1"
	"google.golang.org/api/option"
	run "google.golang.org/api/run/v1"
	secretmanager "google.golang.org/api/secretmanager/v1"
	sqladmin "google.golang.org/api/sqladmin/v1"
	vpcaccess "google.golang.org/api/vpcaccess/v1"
)

// RemoteCheck はリモート実在チェックの結果。Missing は実在しないと確定したリソースの説明
// (verify を失敗させる)。Unchecked は権限不足・API 未到達・認証なしなどで確認できなかった
// ものの説明 (verify を失敗させず、警告として扱う)。後者を失敗にすると、ambient な
// project/region を持つだけの CI のオフライン lint を壊してしまうため区別する。
type RemoteCheck struct {
	Missing   []string
	Unchecked []string
}

// VerifyRemote はマニフェストが参照するリソースが実在するかを API で確認する。Validate
// (ローカルなスキーマ検証) を補完するもので、サービスアカウント・Secret Manager の
// シークレットとその版・VPC コネクタ・Cloud SQL インスタンス・コンテナイメージの実在を
// ADC で確認する。404 (実在しない) のみを Missing として返し、それ以外のエラー
// (クライアント初期化失敗・権限不足・API 無効など) は Unchecked に振り分ける。
//
// region を使うのは VPC コネクタの短縮名を完全なリソース名に補うときだけ。コネクタは
// リージョナルなリソースで、名前だけでは引けない。イメージのロケーションは参照 (ホスト名)
// 自体に入っており、Cloud SQL のプロジェクトは接続名に入っており、IAM も Secret Manager も
// リージョンを取らないので、それ以外に使い道は無い。
//
// opts は NewClient と同じくテストからフェイク API を差し込むための拡張点。
func VerifyRemote(ctx context.Context, project, region string, manifest []byte,
	opts ...option.ClientOption) (*RemoteCheck, error) {
	svc, err := parseManifest(manifest)
	if err != nil {
		return nil, err
	}

	sa := serviceAccountName(svc)
	secrets := secretNames(svc)
	res := &RemoteCheck{}

	if sa != "" {
		iamSvc, err := iam.NewService(ctx, opts...)
		if err != nil {
			res.Unchecked = append(res.Unchecked, fmt.Sprintf("service account %q: %v", sa, err))
		} else {
			// プロジェクト部分はワイルドカード。Cloud Run は別プロジェクトの
			// サービスアカウントを実行 SA にできるので、検証対象のプロジェクトで
			// 固定すると、正当な構成なのに 404 = Missing として verify を失敗させる。
			name := fmt.Sprintf("projects/-/serviceAccounts/%s", sa)
			if _, err := iamSvc.Projects.ServiceAccounts.Get(name).Context(ctx).Do(); err != nil {
				if isNotFound(err) {
					res.Missing = append(res.Missing, fmt.Sprintf("service account %q does not exist", sa))
				} else {
					res.Unchecked = append(res.Unchecked, fmt.Sprintf("service account %q: %v", sa, err))
				}
			}
		}
	}

	checkSecrets(ctx, res, svc, secrets, project, opts...)
	checkVPCConnector(ctx, res, svc, project, region, opts...)
	checkCloudSQL(ctx, res, svc, opts...)
	checkImages(ctx, res, containerImages(svc), opts...)

	return res, nil
}

// vpcConnectorAnnotation / cloudSQLAnnotation は、マニフェストが Cloud Run 以外の
// リソースを参照する 2 つのアノテーション。どちらも「デプロイして初めて落ちる」種類の
// 参照なので、サービスアカウントや Secret と同じ枠で存在を確認する。
const (
	vpcConnectorAnnotation = "run.googleapis.com/vpc-access-connector"
	cloudSQLAnnotation     = "run.googleapis.com/cloudsql-instances"
)

// checkVPCConnector は VPC コネクタの実在を確認する。
//
// アノテーションの値は短縮名 (コネクタ名だけ) と完全なリソース名の両方を取りうる。
// 短縮名の場合はデプロイ先のプロジェクトとリージョンで補う: コネクタはリージョナルな
// リソースなので、ここだけは region が要る。
func checkVPCConnector(ctx context.Context, res *RemoteCheck, svc *run.Service,
	project, region string, opts ...option.ClientOption) {
	connector := templateAnnotation(svc, vpcConnectorAnnotation)
	if connector == "" {
		return
	}
	name := connector
	if !strings.HasPrefix(name, "projects/") {
		if region == "" {
			res.Unchecked = append(res.Unchecked,
				fmt.Sprintf("VPC connector %q: no region to resolve the short name against", connector))
			return
		}
		name = fmt.Sprintf("projects/%s/locations/%s/connectors/%s", project, region, connector)
	}

	svcAPI, err := vpcaccess.NewService(ctx, opts...)
	if err != nil {
		res.Unchecked = append(res.Unchecked, fmt.Sprintf("VPC connector %q: %v", connector, err))
		return
	}
	if _, err := svcAPI.Projects.Locations.Connectors.Get(name).Context(ctx).Do(); err != nil {
		if isNotFound(err) {
			res.Missing = append(res.Missing, fmt.Sprintf("VPC connector %q does not exist", connector))
			return
		}
		res.Unchecked = append(res.Unchecked, fmt.Sprintf("VPC connector %q: %v", connector, err))
	}
}

// checkCloudSQL は接続先の Cloud SQL インスタンスの実在を確認する。値は
// "<project>:<region>:<instance>" のカンマ区切り。プロジェクトは接続名から取るので、
// 別プロジェクトのインスタンスを誤って Missing にしない。
func checkCloudSQL(ctx context.Context, res *RemoteCheck, svc *run.Service, opts ...option.ClientOption) {
	raw := templateAnnotation(svc, cloudSQLAnnotation)
	if raw == "" {
		return
	}

	var sqlSvc *sqladmin.Service
	for _, entry := range strings.Split(raw, ",") {
		conn := strings.TrimSpace(entry)
		if conn == "" {
			continue
		}
		// 形が違うものは「無い」ではなく「確かめられない」。Cloud Run 側が受け取る
		// 形式は決まっているが、誤判定して verify を落とすより黙らないほうを選ぶ。
		//
		// 右から切るのは、ドメインスコープのプロジェクト (example.com:my-project) が
		// それ自体に ":" を含むため。左から 3 分割すると、正当な接続名が毎回
		// 「形が違う」警告になる。
		project, instance, ok := splitConnectionName(conn)
		if !ok {
			res.Unchecked = append(res.Unchecked,
				fmt.Sprintf("Cloud SQL instance %q: not in <project>:<region>:<instance> form", conn))
			continue
		}
		if sqlSvc == nil {
			created, err := sqladmin.NewService(ctx, opts...)
			if err != nil {
				res.Unchecked = append(res.Unchecked, fmt.Sprintf("Cloud SQL instance %q: %v", conn, err))
				continue
			}
			sqlSvc = created
		}
		if _, err := sqlSvc.Instances.Get(project, instance).Context(ctx).Do(); err != nil {
			if isNotFound(err) {
				res.Missing = append(res.Missing, fmt.Sprintf("Cloud SQL instance %q does not exist", conn))
				continue
			}
			res.Unchecked = append(res.Unchecked, fmt.Sprintf("Cloud SQL instance %q: %v", conn, err))
		}
	}
}

// splitConnectionName は <project>:<region>:<instance> をプロジェクトとインスタンスに
// 分ける。ドメインスコープのプロジェクト (example.com:my-project:<region>:<instance>) も
// 扱えるよう、右の 2 つを region / instance として切り、残りをプロジェクトとする。
func splitConnectionName(conn string) (project, instance string, ok bool) {
	parts := strings.Split(conn, ":")
	if len(parts) < 3 {
		return "", "", false
	}
	instance = parts[len(parts)-1]
	region := parts[len(parts)-2]
	project = strings.Join(parts[:len(parts)-2], ":")
	if project == "" || region == "" || instance == "" {
		return "", "", false
	}
	return project, instance, true
}

// checkSecrets はシークレットの実在と、参照している *バージョン* の実在を確認する。
//
// バージョンを別に見るのは、存在するシークレットの消えた版 (あるいは打ち間違えた番号)
// がデプロイして初めて落ちるため。"latest" もそのまま解決できる。
//
// シークレット自体が見つからなかった場合、その版は問い合わせない。「secret X does not
// exist」と「secret X has no version latest」を両方並べても分かることは増えず、
// 本当の原因が埋もれるだけになる。
func checkSecrets(ctx context.Context, res *RemoteCheck, svc *run.Service, secrets []string,
	project string, opts ...option.ClientOption) {
	if len(secrets) == 0 {
		return
	}
	aliases := secretAliases(svc)
	versions := versionsBySecret(svc)

	smSvc, err := secretmanager.NewService(ctx, opts...)
	if err != nil {
		for _, s := range secrets {
			res.Unchecked = append(res.Unchecked, fmt.Sprintf("secret %q: %v", s, err))
		}
		return
	}

	for _, s := range secrets {
		name := secretResourceName(project, s, aliases)
		if _, err := smSvc.Projects.Secrets.Get(name).Context(ctx).Do(); err != nil {
			if isNotFound(err) {
				res.Missing = append(res.Missing, fmt.Sprintf("secret %q does not exist", s))
			} else {
				res.Unchecked = append(res.Unchecked, fmt.Sprintf("secret %q: %v", s, err))
			}
			continue
		}
		for _, version := range versions[s] {
			versionName := fmt.Sprintf("%s/versions/%s", name, version)
			got, err := smSvc.Projects.Secrets.Versions.Get(versionName).Context(ctx).Do()
			if err != nil {
				if isNotFound(err) {
					res.Missing = append(res.Missing,
						fmt.Sprintf("secret %q has no version %q", s, version))
					continue
				}
				res.Unchecked = append(res.Unchecked,
					fmt.Sprintf("secret %q version %q: %v", s, version, err))
				continue
			}
			// 破棄・無効化された版も get は 200 で返す (読めなくなるのは access の方)。
			// 状態を見ないと「消えた版を指したまま素通り」になり、この検査を足した
			// 意味がなくなる。
			if state := got.State; state != "" && state != secretVersionEnabled {
				res.Missing = append(res.Missing,
					fmt.Sprintf("secret %q version %q is %s, so it cannot be read", s, version, state))
			}
		}
	}
}

// versionsBySecret はシークレットごとの参照バージョンを重複なく集める。
func versionsBySecret(svc *run.Service) map[string][]string {
	out := make(map[string][]string)
	seen := make(map[secretVersionRef]bool)
	for _, ref := range secretVersionRefs(svc) {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		out[ref.Secret] = append(out[ref.Secret], ref.Version)
	}
	return out
}

// secretVersionEnabled は読み出せるバージョンの状態。これ以外 (DISABLED / DESTROYED) は
// 参照できないので、実在しない版と同じ扱いにする。
const secretVersionEnabled = "ENABLED"

// secretVersionRef はシークレットとそのバージョンの組。
type secretVersionRef struct {
	Secret  string
	Version string
}

// secretVersionRefs はマニフェストが参照する (シークレット, バージョン) の組を重複なく
// 集める。env の secretKeyRef.key と、secret ボリュームの items[].key がバージョンにあたる。
// バージョンの指定が無いものは Cloud Run と同じく "latest" として扱う。
func secretVersionRefs(svc *run.Service) []secretVersionRef {
	spec := templateSpec(svc)
	if spec == nil {
		return nil
	}

	seen := make(map[secretVersionRef]bool)
	var out []secretVersionRef
	add := func(secret, version string) {
		if secret == "" {
			return
		}
		// 版は key に入るのが普通だが、name が
		// projects/<p>/secrets/<s>/versions/<v> の形なら中に埋まっている
		// (secretResourceName はこの形を明示的に扱う)。key が無ければそちらを使う。
		if version == "" {
			version = versionFromSecretPath(secret)
		}
		if version == "" {
			version = "latest"
		}
		ref := secretVersionRef{Secret: secret, Version: version}
		if !seen[ref] {
			seen[ref] = true
			out = append(out, ref)
		}
	}

	for _, c := range spec.Containers {
		if c == nil {
			continue
		}
		for _, e := range c.Env {
			if e != nil && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
				add(e.ValueFrom.SecretKeyRef.Name, e.ValueFrom.SecretKeyRef.Key)
			}
		}
	}
	for _, v := range spec.Volumes {
		if v == nil || v.Secret == nil {
			continue
		}
		if len(v.Secret.Items) == 0 {
			add(v.Secret.SecretName, "")
			continue
		}
		for _, item := range v.Secret.Items {
			if item != nil {
				add(v.Secret.SecretName, item.Key)
			}
		}
	}
	return out
}

// versionFromSecretPath は projects/<p>/secrets/<s>/versions/<v> 形式の名前から版を返す。
// その形でなければ空文字列。
func versionFromSecretPath(name string) string {
	const marker = "/versions/"
	if i := strings.Index(name, marker); i >= 0 {
		return name[i+len(marker):]
	}
	return ""
}

// templateAnnotation は spec.template.metadata のアノテーションを nil セーフに読む。
func templateAnnotation(svc *run.Service, key string) string {
	meta := templateMeta(svc)
	if meta == nil {
		return ""
	}
	return strings.TrimSpace(meta.Annotations[key])
}

// checkImages は containers[].image の実在を Artifact Registry で確認し、結果を res に足す。
//
// 確認できるのは Artifact Registry のイメージだけ。gcr.io には相当する API が無く、
// Docker Hub その他は端から範囲外なので、**黙って飛ばす**。ここを Unchecked に入れると
// Docker Hub のイメージを使っているだけで毎回 warning が出て、警告そのものが読み飛ばされる
// ようになる。Unchecked は「確認しに行って決められなかった」ときのために取っておく。
// 何を確認できるかは README と verify の --help に書いてある。
//
// 「確認できない = 存在しない」に倒さないことがこの関数の要件 (#23 と同じ壊れ方をする)。
// 実 API で確かめた挙動:
//   - リポジトリ / パッケージ / タグ / ダイジェストのいずれが無くても 404
//   - 存在しない (またはアクセスできない) プロジェクトは 403 なので Missing にならない
//   - 公開イメージ (us-docker.pkg.dev/cloudrun/container/hello) は通常の ADC で引ける
func checkImages(ctx context.Context, res *RemoteCheck, images []string, opts ...option.ClientOption) {
	var refs []imageRef
	for _, img := range images {
		if ref := parseImageRef(img); ref.IsArtifactRegistry() {
			refs = append(refs, ref)
		}
	}
	if len(refs) == 0 {
		return
	}

	// クライアントの生成は確認対象があるときだけ。イメージが全部 gcr.io のマニフェストで
	// Artifact Registry API の有効化を要求したくない。
	arSvc, err := artifactregistry.NewService(ctx, opts...)
	if err != nil {
		for _, ref := range refs {
			res.Unchecked = append(res.Unchecked, fmt.Sprintf("image %q: %v", ref.Raw, err))
		}
		return
	}
	for _, ref := range refs {
		name := ref.resourceName()
		var getErr error
		if ref.Digest != "" {
			_, getErr = arSvc.Projects.Locations.Repositories.DockerImages.Get(name).Context(ctx).Do()
		} else {
			_, getErr = arSvc.Projects.Locations.Repositories.Packages.Tags.Get(name).Context(ctx).Do()
		}
		switch {
		case getErr == nil:
		case isNotFound(getErr):
			res.Missing = append(res.Missing, fmt.Sprintf("image %q does not exist", ref.Raw))
		default:
			res.Unchecked = append(res.Unchecked, fmt.Sprintf("image %q: %v", ref.Raw, getErr))
		}
	}
}

// serviceAccountName はマニフェストの実行サービスアカウントを nil セーフに取り出す。
func serviceAccountName(svc *run.Service) string {
	spec := templateSpec(svc)
	if spec == nil {
		return ""
	}
	return spec.ServiceAccountName
}

// secretNames はマニフェストが参照する Secret Manager シークレット名を重複なく集める。
// env の secretKeyRef と secret ボリュームの両方を見る。
func secretNames(svc *run.Service) []string {
	spec := templateSpec(svc)
	if spec == nil {
		return nil
	}

	seen := make(map[string]bool)
	var out []string
	add := func(n string) {
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}

	for _, c := range spec.Containers {
		if c == nil {
			continue
		}
		for _, e := range c.Env {
			if e != nil && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
				add(e.ValueFrom.SecretKeyRef.Name)
			}
		}
	}
	for _, v := range spec.Volumes {
		if v != nil && v.Secret != nil {
			add(v.Secret.SecretName)
		}
	}
	return out
}

// secretAliasAnnotation は別プロジェクトのシークレット参照のエイリアス定義を持つ
// アノテーションキー。値は "<alias>:projects/<p>/secrets/<s>" をカンマ区切りで並べたもの。
const secretAliasAnnotation = "run.googleapis.com/secrets"

// secretAliases は spec.template.metadata の run.googleapis.com/secrets アノテーションを
// パースし、エイリアス名 -> 実体パス (projects/<p>/secrets/<s>) のマップを返す。
// 別プロジェクトのシークレットは secretKeyRef.name にエイリアスだけが入り、実体パスは
// このアノテーションにあるため、これを引かないと存在チェックが誤判定する。
func secretAliases(svc *run.Service) map[string]string {
	if svc.Spec == nil || svc.Spec.Template == nil || svc.Spec.Template.Metadata == nil {
		return nil
	}
	raw := svc.Spec.Template.Metadata.Annotations[secretAliasAnnotation]
	if raw == "" {
		return nil
	}
	out := make(map[string]string)
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		// "<alias>:projects/<p>/secrets/<s>" を最初の ":" で分割する (実体パスに ":" は無い)。
		if i := strings.Index(entry, ":"); i > 0 {
			out[entry[:i]] = entry[i+1:]
		}
	}
	return out
}

// secretResourceName はシークレット名を Secret Manager の resource 名に整える。
// 既に projects/.../secrets/... 形式ならそのまま (末尾の /versions/... は落とす)。
// 別プロジェクトのエイリアスは aliases から実体パスへ解決する。それ以外は同一プロジェクト
// のシークレットとみなす。
func secretResourceName(project, name string, aliases map[string]string) string {
	if strings.HasPrefix(name, "projects/") {
		if i := strings.Index(name, "/versions/"); i >= 0 {
			return name[:i]
		}
		return name
	}
	if path, ok := aliases[name]; ok {
		return secretResourceName(project, path, nil)
	}
	return fmt.Sprintf("projects/%s/secrets/%s", project, name)
}
