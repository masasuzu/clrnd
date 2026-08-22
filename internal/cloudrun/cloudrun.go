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
var serverManagedAnnotations = []string{
	"run.googleapis.com/operation-id",
	"run.googleapis.com/ingress-status",
	"run.googleapis.com/urls",
	"serving.knative.dev/creator",
	"serving.knative.dev/lastModifier",
}

// サーバ側が付与する read-only なラベル。
var serverManagedLabels = []string{
	"client.knative.dev/nonce",
	"run.googleapis.com/startupProbeType",
}

// metadata 直下の read-only フィールド。
var serverManagedMetaFields = []string{
	"creationTimestamp",
	"generation",
	"resourceVersion",
	"selfLink",
	"uid",
	"namespace",
}

// DeployPlan は適用予定の内容。Plan で算出し、Apply で適用する。
type DeployPlan struct {
	Service string // サービス名
	Create  bool   // 未存在で新規作成になるか
	Diff    string // live と desired の統一 diff (差分が無ければ空)

	client  *Client
	desired *run.Service
}

// Plan はマニフェストを検証し、live サービスとの差分を算出する (変更はしない)。
func (c *Client) Plan(ctx context.Context, service string, manifest []byte) (*DeployPlan, error) {
	svc, err := parseManifest(manifest)
	if err != nil {
		return nil, err
	}
	if err := validate(svc, service); err != nil {
		return nil, err
	}
	// 送信先プロジェクトと body の namespace を一致させる。
	if svc.Metadata != nil {
		svc.Metadata.Namespace = c.project
	}

	plan := &DeployPlan{Service: service, client: c, desired: svc}

	current, getErr := c.api.Namespaces.Services.Get(c.serviceName(service)).Context(ctx).Do()
	if getErr != nil {
		if !isNotFound(getErr) {
			return nil, fmt.Errorf("failed to check service %q: %w", service, getErr)
		}
		// 未存在: 新規作成。current 側は空として diff を取る。
		plan.Create = true
		current = nil
	}

	if plan.Diff, err = compareServices(current, svc, "live/"+service, service); err != nil {
		return nil, err
	}
	return plan, nil
}

// Apply は Plan の内容を Cloud Run に適用する。dryRun が true の場合はサーバ側で検証のみ行う。
// dryRun が false のときは DryRun を呼ばない (空文字を渡すと dryRun= という空のクエリ
// パラメータが送られてしまうため)。
func (p *DeployPlan) Apply(ctx context.Context, dryRun bool) error {
	if p.Create {
		call := p.client.api.Namespaces.Services.Create(p.client.parent(), p.desired)
		if dryRun {
			call = call.DryRun(dryRunAll)
		}
		if _, err := call.Context(ctx).Do(); err != nil {
			return fmt.Errorf("failed to create service %q: %w", p.Service, err)
		}
		return nil
	}

	call := p.client.api.Namespaces.Services.ReplaceService(p.client.serviceName(p.Service), p.desired)
	if dryRun {
		call = call.DryRun(dryRunAll)
	}
	if _, err := call.Context(ctx).Do(); err != nil {
		return fmt.Errorf("failed to update service %q: %w", p.Service, err)
	}
	return nil
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

// Compare は live サービス (current) とローカルのマニフェストを同じ正規化にそろえて
// 統一 diff を返す。current が nil ならサービス未存在として、desired 全体が追加された
// diff になる。`clrnd diff` と `clrnd deploy` が同一の差分を表示するための共通経路。
// マニフェストは厳密にパースするので、未知フィールドはここでエラーになる。
func Compare(current *run.Service, manifest []byte, currentName, desiredName string) (string, error) {
	desired, err := parseManifest(manifest)
	if err != nil {
		return "", err
	}
	return compareServices(current, desired, currentName, desiredName)
}

// compareServices は Compare と Plan が共有する比較の実装。
func compareServices(current, desired *run.Service, currentName, desiredName string) (string, error) {
	alignRevisionName(current, desired)

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

// StripRevisionName は spec.template.metadata.name (リビジョン名) を取り除く。
// Cloud Run はリビジョン名を省略するとサーバ側で自動採番するが、明示すると同名リビジョン
// の再作成ができない。live から起こしたマニフェストにそのまま残すと、テンプレートを変えた
// 2 回目以降の deploy が失敗するため、init が scaffold するマニフェストからは落とす。
func StripRevisionName(svc *run.Service) {
	if meta := templateMeta(svc); meta != nil {
		meta.Name = ""
	}
}

// alignRevisionName は desired がリビジョン名を指定していないとき、比較対象の current から
// もリビジョン名を取り除く (current を書き換える)。
//
// Cloud Run は取得時に必ず実際のリビジョン名を埋めて返すため、ローカルが指定していない
// 限りこれはサーバ管理フィールドと同じ扱いにするのが正しい。そうしないと、リビジョン名を
// 書かないマニフェストでは永久に消えない差分が diff に出続ける。
// ローカルが明示している場合は両側に残し、差分として見せる。
func alignRevisionName(current, desired *run.Service) {
	if current == nil || revisionName(desired) != "" {
		return
	}
	StripRevisionName(current)
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
