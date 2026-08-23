// Package cloudrun は Cloud Run Admin API へのアクセスとマニフェストの整形を提供する。
package cloudrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pmezard/go-difflib/difflib"
	"google.golang.org/api/googleapi"
	run "google.golang.org/api/run/v1"
	"sigs.k8s.io/yaml"
)

const (
	manifestAPIVersion = "serving.knative.dev/v1"
	manifestKind       = "Service"
	// dryRunAll は API の dryRun クエリパラメータで「検証のみ」を指示する値。
	dryRunAll = "all"
)

// サーバ側が付与する read-only なアノテーション。デプロイ用マニフェストには不要。
// metadata 直下と spec.template.metadata の両方に対して使う。
//
// client-name / client-version は「最後に書き込んだツール」を記録するもので、設定では
// ない。gcloud で作られたサービスを init で取り込むとマニフェストに "gcloud" が焼き付き、
// 以後の clrnd deploy がそれを送り返し続ける。手書きマニフェストでは逆に、消えない削除
// 差分として出る。clrnd はこれを管理しない。
var serverManagedAnnotations = []string{
	"run.googleapis.com/operation-id",
	"run.googleapis.com/ingress-status",
	"run.googleapis.com/urls",
	"run.googleapis.com/client-name",
	"run.googleapis.com/client-version",
	"serving.knative.dev/creator",
	"serving.knative.dev/lastModifier",
}

// サーバ側が付与する read-only なラベル。metadata 直下と spec.template.metadata の
// 両方に対して使う (cloud.googleapis.com/location は実際には metadata 直下にだけ付くが、
// テンプレート側から消えて困るものではないので一覧を分けていない)。
var serverManagedLabels = []string{
	"client.knative.dev/nonce",
	"run.googleapis.com/startupProbeType",
	"cloud.googleapis.com/location",
}

// metadata 直下の read-only フィールド。run.ObjectMeta のうち、クライアントが書かない
// (書いても意味が無い) ものを列挙する。削除中のサービスや、他のコントローラ (Terraform,
// Config Connector など) が管理しているサービスを init で取り込んだときに、これらが
// scaffold されたマニフェストに残って次の deploy でそのまま送り返されるのを防ぐ。
var serverManagedMetaFields = []string{
	"creationTimestamp",
	"generation",
	"resourceVersion",
	"selfLink",
	"uid",
	"namespace",
	"deletionTimestamp",
	"deletionGracePeriodSeconds",
	"finalizers",
	"ownerReferences",
	"generateName",
	"clusterName",
}

// DeployPlan は適用予定の内容。Plan で算出し、Apply で適用する。
type DeployPlan struct {
	Service string // サービス名
	Create  bool   // 未存在で新規作成になるか
	Diff    string // live と desired の統一 diff (差分が無ければ空)

	client  *Client
	desired *run.Service
}

// PlanOptions は差分の取り方に関する任意設定。ゼロ値が既定の挙動。
type PlanOptions struct {
	// ResolveDefaults が true なら、差分を取る前にサーバ側の dry-run を通して
	// 既定値まで埋めた desired を作る。Cloud Run は作成時に多くのフィールドへ
	// 既定値を入れるため、手書きの最小マニフェストは何もしなくても差分が出続ける
	// (issue #11)。これを有効にすると、その分が両側で揃って消える。
	//
	// dry-run は書き込み系の API なので、読むだけの権限では使えない。CLI は既定で
	// これを有効にし、--no-server-defaults で外せるようにしている (この構造体の
	// ゼロ値は「解決しない」のままで、live 由来の定義を扱う rollback / refresh は
	// 既に既定値が入っているので解決を必要としない)。
	// 適用に送るのは常に元の desired で、サーバが埋めた値を書き戻すことはしない。
	ResolveDefaults bool
}

// Plan はマニフェストを検証し、live サービスとの差分を算出する (変更はしない)。
func (c *Client) Plan(ctx context.Context, service string, manifest []byte, opts PlanOptions) (*DeployPlan, error) {
	svc, err := parseManifest(manifest)
	if err != nil {
		return nil, err
	}
	if err := validate(svc, service); err != nil {
		return nil, err
	}
	return c.PlanService(ctx, service, svc, opts)
}

// PlanService は desired のサービス定義をそのまま使って live との差分を算出する。
// マニフェストを経由しない rollback や refresh のように、live を編集して適用する
// 経路のための入口。desired の metadata.namespace は送信先に合わせて書き換える。
func (c *Client) PlanService(ctx context.Context, service string, desired *run.Service, opts PlanOptions) (*DeployPlan, error) {
	if desired == nil {
		return nil, errors.New("no desired service to plan")
	}
	c.setNamespace(desired)

	plan := &DeployPlan{Service: service, client: c, desired: desired}

	current, getErr := c.api.Namespaces.Services.Get(c.serviceName(service)).Context(ctx).Do()
	if getErr != nil {
		if !isNotFound(getErr) {
			return nil, fmt.Errorf("failed to check service %q: %w", service, getErr)
		}
		// 未存在: 新規作成。current 側は空として diff を取る。
		plan.Create = true
		current = nil
	}

	// 差分に使う desired だけを既定値まで解決する。plan.desired (適用に送るもの) は
	// 元のままにしておき、サーバが埋めた値を書き戻さない。
	compared := desired
	if opts.ResolveDefaults {
		resolved, err := c.resolveDefaults(ctx, service, desired, plan.Create)
		if err != nil {
			return nil, err
		}
		compared = resolved
	}

	diff, err := compareServices(current, compared, "live/"+service, service)
	if err != nil {
		return nil, err
	}
	plan.Diff = diff
	return plan, nil
}

// CompareManifest は live サービスとローカルのマニフェストの差分を返す。diff 用の入口で、
// サービスの取得と (必要なら) 既定値の解決をまとめて行う。
func (c *Client) CompareManifest(ctx context.Context, service string, manifest []byte,
	desiredLabel string, opts PlanOptions) (string, error) {
	desired, err := parseManifest(manifest)
	if err != nil {
		return "", err
	}
	// deploy と同じ検証を通す。ここを --server-defaults のときだけにすると、
	// deploy が拒否する入力 (metadata.name がサービス名と違う等) を diff だけが
	// 受け入れ、「名前を変更できるかのような差分」を出してしまう。
	if err := validate(desired, service); err != nil {
		return "", err
	}
	// 送信先に合わせる。--server-defaults の dry-run は本物の書き込みと同じ検証を
	// 受けるので、deploy と同じ前処理を通しておかないと diff だけが弾かれる。
	c.setNamespace(desired)

	current, err := c.GetService(ctx, service)
	if err != nil {
		if !isNotFound(err) {
			return "", err
		}
		// まだ作られていないサービス。PlanService と同じく「全部追加」として扱う。
		// ここで 404 を返していたので、README が勧める init 前の
		// 「マニフェストを書く → diff → deploy」が初回だけ通らなかった。
		current = nil
	}

	if opts.ResolveDefaults {
		// 未存在なら dry-run も Create でなければ 404 になる。
		if desired, err = c.resolveDefaults(ctx, service, desired, current == nil); err != nil {
			return "", err
		}
	}
	return compareServices(current, desired, "live/"+service, desiredLabel)
}

// setNamespace は送信先プロジェクトと body の namespace を一致させる。
// 適用も dry-run もこれを通した定義で行う。
func (c *Client) setNamespace(svc *run.Service) {
	if svc != nil && svc.Metadata != nil {
		svc.Metadata.Namespace = c.project
	}
}

// resolveDefaults はサーバ側の dry-run を通して、Cloud Run が埋める既定値まで入った
// サービス定義を得る。何も変更しない (dryRun=all)。
func (c *Client) resolveDefaults(ctx context.Context, service string, desired *run.Service, create bool) (*run.Service, error) {
	var (
		resolved *run.Service
		err      error
	)
	if create {
		resolved, err = c.api.Namespaces.Services.Create(c.parent(), desired).
			DryRun(dryRunAll).Context(ctx).Do()
	} else {
		resolved, err = c.api.Namespaces.Services.ReplaceService(c.serviceName(service), desired).
			DryRun(dryRunAll).Context(ctx).Do()
	}
	if err != nil {
		// 原因は権限とは限らない (マニフェストの内容が拒否された、途中で削除された等)。
		// 断定せず、この経路が何をしているかだけを添える。
		return nil, fmt.Errorf(
			"failed to resolve server defaults for service %q: %w "+
				"(resolving them performs a dry-run update, which needs permission to update the "+
				"service; pass --no-server-defaults to compare without one)", service, err)
	}
	return resolved, nil
}

// Apply は Plan の内容を Cloud Run に適用し、適用後のサービスを返す。dryRun が true の
// 場合はサーバ側で検証のみ行う。dryRun が false のときは DryRun を呼ばない (空文字を渡すと
// dryRun= という空のクエリパラメータが送られてしまうため)。
//
// 戻り値の metadata.generation は「今適用した世代」なので、Wait でその世代の
// ロールアウトだけを待つのに使える。
func (p *DeployPlan) Apply(ctx context.Context, dryRun bool) (*run.Service, error) {
	if p.Create {
		call := p.client.api.Namespaces.Services.Create(p.client.parent(), p.desired)
		if dryRun {
			call = call.DryRun(dryRunAll)
		}
		applied, err := call.Context(ctx).Do()
		if err != nil {
			return nil, fmt.Errorf("failed to create service %q: %w", p.Service, err)
		}
		return applied, nil
	}

	call := p.client.api.Namespaces.Services.ReplaceService(p.client.serviceName(p.Service), p.desired)
	if dryRun {
		call = call.DryRun(dryRunAll)
	}
	applied, err := call.Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("failed to update service %q: %w", p.Service, err)
	}
	return applied, nil
}

// DeleteService はサービスを削除する。dryRun が true の場合はサーバ側で検証のみ行う。
// 取り消せない操作なので、呼び出し側で確認を取ること。
func (c *Client) DeleteService(ctx context.Context, service string, dryRun bool) error {
	call := c.api.Namespaces.Services.Delete(c.serviceName(service))
	if dryRun {
		call = call.DryRun(dryRunAll)
	}
	if _, err := call.Context(ctx).Do(); err != nil {
		return fmt.Errorf("failed to delete service %q: %w", service, err)
	}
	return nil
}

// AppliedGeneration は Apply の戻り値から metadata.generation を nil セーフに取り出す。
// 取れなければ 0 を返し、その場合 Wait は世代を問わずに Ready だけを見る。
func AppliedGeneration(applied *run.Service) int64 {
	if applied == nil || applied.Metadata == nil {
		return 0
	}
	return applied.Metadata.Generation
}

// isNotFound は googleapi の 404 エラーかどうかを判定する。
func isNotFound(err error) bool {
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		return gerr.Code == 404
	}
	return false
}

// ToManifest はサーバ側が付与する read-only フィールドを取り除き、デプロイに使える
// Knative 形式の YAML マニフェストを返す。
func ToManifest(obj *run.Service) ([]byte, error) {
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("failed to sanitize the manifest: %w", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("failed to sanitize the manifest: %w", err)
	}
	sanitizeMap(m)

	manifest, err := yaml.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("failed to convert the manifest to YAML: %w", err)
	}
	return manifest, nil
}

// compareServices は差分を取る各経路が共有する比較の実装。引数は書き換えない。
func compareServices(current, desired *run.Service, currentName, desiredName string) (string, error) {
	current = alignRevisionName(current, desired)

	desiredYAML, err := ToManifest(desired)
	if err != nil {
		return "", err
	}

	var currentYAML []byte
	if current != nil {
		if currentYAML, err = ToManifest(current); err != nil {
			return "", err
		}
	}
	return Diff(currentYAML, desiredYAML, currentName, desiredName)
}

// CheckSyntax はマニフェストが run.Service として厳密にパースできるかだけを確認する。
// API アクセスを伴う処理の前にローカルの問題を先に出すために使う (Validate と違い、
// サービス名の一致や必須フィールドは見ない)。
func CheckSyntax(manifest []byte) error {
	_, err := parseManifest(manifest)
	return err
}

// Validate はローカルのマニフェストが Cloud Run のサービス定義として妥当かを検証する。
// API へはアクセスせず、構造とデプロイに必須のフィールドだけを確認する。問題が無ければ
// nil を、複数の問題があればまとめたエラーを返す。
func Validate(manifest []byte, service string) error {
	svc, err := parseManifest(manifest)
	if err != nil {
		return err
	}
	return validate(svc, service)
}

// parseManifest はマニフェストを run.Service に厳密にパースする。UnmarshalStrict は
// 未知フィールド (フィールド名の打ち間違いなど) も検出する。
func parseManifest(manifest []byte) (*run.Service, error) {
	var svc run.Service
	if err := yaml.UnmarshalStrict(manifest, &svc); err != nil {
		return nil, fmt.Errorf("failed to parse the manifest: %w", err)
	}
	return &svc, nil
}

// validate はパース済みのサービス定義を検証する。
func validate(svc *run.Service, service string) error {
	var errs []error
	if svc.ApiVersion != manifestAPIVersion {
		errs = append(errs, fmt.Errorf("apiVersion must be %q, got %q", manifestAPIVersion, svc.ApiVersion))
	}
	if svc.Kind != manifestKind {
		errs = append(errs, fmt.Errorf("kind must be %q, got %q", manifestKind, svc.Kind))
	}

	switch {
	case svc.Metadata == nil || svc.Metadata.Name == "":
		errs = append(errs, errors.New("metadata.name is required"))
	case svc.Metadata.Name != service:
		errs = append(errs, fmt.Errorf("metadata.name %q does not match service argument %q", svc.Metadata.Name, service))
	}

	containers := serviceContainers(svc)
	if len(containers) == 0 {
		errs = append(errs, errors.New("spec.template.spec.containers must define at least one container"))
	}
	for i, c := range containers {
		switch {
		case c == nil:
			errs = append(errs, fmt.Errorf("spec.template.spec.containers[%d] must not be null", i))
		case c.Image == "":
			errs = append(errs, fmt.Errorf("spec.template.spec.containers[%d].image is required", i))
		}
	}

	return errors.Join(errs...)
}

// templateSpec はサービス定義の spec.template.spec (RevisionSpec) を nil セーフに取り出す。
// コンテナ・サービスアカウント・ボリュームなど template 配下を見る処理で共有する。
func templateSpec(svc *run.Service) *run.RevisionSpec {
	if svc == nil || svc.Spec == nil || svc.Spec.Template == nil {
		return nil
	}
	return svc.Spec.Template.Spec
}

// templateMeta はサービス定義の spec.template.metadata を nil セーフに取り出す。
func templateMeta(svc *run.Service) *run.ObjectMeta {
	if svc == nil || svc.Spec == nil || svc.Spec.Template == nil {
		return nil
	}
	return svc.Spec.Template.Metadata
}

// revisionName は spec.template.metadata.name (リビジョン名) を nil セーフに取り出す。
func revisionName(svc *run.Service) string {
	meta := templateMeta(svc)
	if meta == nil {
		return ""
	}
	return meta.Name
}

// RevisionName はマニフェストが固定しているリビジョン名を返す。指定が無ければ空文字列。
func RevisionName(manifest []byte) (string, error) {
	svc, err := parseManifest(manifest)
	if err != nil {
		return "", err
	}
	return revisionName(svc), nil
}

// WithoutRevisionName は spec.template.metadata.name (リビジョン名) を取り除いたサービスを
// 返す。引数は書き換えず、変更が必要な経路 (Spec / Template / Metadata) だけを浅くコピーする。
//
// Cloud Run はリビジョン名を省略するとサーバ側で自動採番するが、明示すると設定の異なる
// 同名リビジョンを作れない。live から起こしたマニフェストにそのまま残すと、テンプレートを
// 変えた 2 回目以降の deploy が失敗するため、init が scaffold するマニフェストからは落とす。
func WithoutRevisionName(svc *run.Service) *run.Service {
	if revisionName(svc) == "" {
		return svc
	}
	meta := *svc.Spec.Template.Metadata
	meta.Name = ""
	tmpl := *svc.Spec.Template
	tmpl.Metadata = &meta
	spec := *svc.Spec
	spec.Template = &tmpl
	out := *svc
	out.Spec = &spec
	return &out
}

// alignRevisionName は desired がリビジョン名を指定していないとき、比較に使う current から
// リビジョン名を落としたものを返す。引数は書き換えない。
//
// Cloud Run は取得時に必ず実際のリビジョン名を埋めて返すため、ローカルが指定していない
// 限りこれはサーバ管理フィールドと同じ扱いにするのが正しい。そうしないと、リビジョン名を
// 書かないマニフェストでは永久に消えない差分が diff に出続ける。
// ローカルが明示している場合は両側に残し、差分として見せる。
func alignRevisionName(current, desired *run.Service) *run.Service {
	if current == nil || revisionName(desired) != "" {
		return current
	}
	return WithoutRevisionName(current)
}

// serviceContainers はサービス定義からコンテナ一覧を nil セーフに取り出す。
func serviceContainers(svc *run.Service) []*run.Container {
	spec := templateSpec(svc)
	if spec == nil {
		return nil
	}
	return spec.Containers
}

// Diff は current と desired の統一 diff を返す。差分が無ければ空文字列を返す。
func Diff(current, desired []byte, currentName, desiredName string) (string, error) {
	d := difflib.UnifiedDiff{
		A:        difflib.SplitLines(string(current)),
		B:        difflib.SplitLines(string(desired)),
		FromFile: currentName,
		ToFile:   desiredName,
		Context:  3,
	}
	out, err := difflib.GetUnifiedDiffString(d)
	if err != nil {
		return "", fmt.Errorf("failed to compute the diff: %w", err)
	}
	return out, nil
}

// sanitizeMap はサーバ側が付与する read-only なフィールドを map から取り除く。
func sanitizeMap(m map[string]interface{}) {
	// status はすべてサーバ側の状態情報なので丸ごと削除する。
	delete(m, "status")

	// metadata 直下の read-only フィールドとサーバ管理アノテーションを削除する。
	if meta, ok := m["metadata"].(map[string]interface{}); ok {
		for _, k := range serverManagedMetaFields {
			delete(meta, k)
		}
		deleteMapKeys(meta, "annotations", serverManagedAnnotations)
		// Cloud Run は全サービスに cloud.googleapis.com/location を付ける。これを消さないと、
		// 書いていない手書きマニフェストでは永久に削除差分として出続ける。
		deleteMapKeys(meta, "labels", serverManagedLabels)
	}

	// spec.template.metadata のサーバ管理ラベル/アノテーションを削除する。
	if spec, ok := m["spec"].(map[string]interface{}); ok {
		if tmpl, ok := spec["template"].(map[string]interface{}); ok {
			if tmeta, ok := tmpl["metadata"].(map[string]interface{}); ok {
				deleteMapKeys(tmeta, "annotations", serverManagedAnnotations)
				deleteMapKeys(tmeta, "labels", serverManagedLabels)
				// 中身が空になった metadata は出力しない。ローカルのマニフェストには
				// 通常 spec.template.metadata 自体が無いので、空オブジェクトを残すと
				// "metadata: {}" が消えない差分として出続ける。
				if len(tmeta) == 0 {
					delete(tmpl, "metadata")
				}
			}
		}
	}
}

// deleteMapKeys は parent[field] (map) から指定キーを削除し、空になったら field 自体も削除する。
func deleteMapKeys(parent map[string]interface{}, field string, keys []string) {
	child, ok := parent[field].(map[string]interface{})
	if !ok {
		return
	}
	for _, k := range keys {
		delete(child, k)
	}
	if len(child) == 0 {
		delete(parent, field)
	}
}
