// Package render はマニフェストを text/template として評価し、Terraform state の値で
// プレースホルダーを埋める。ecspresso の tfstate 連携と同様の仕組み。
package render

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"text/template"

	"github.com/fujiwara/tfstate-lookup/tfstate"
)

// DefaultStateName は {{ tfstate "addr" }} (名前省略) のときに使う state 名。
const DefaultStateName = "default"

// Source は名前付きの Terraform state の場所を表す。Location はローカルパスまたは
// gs://, s3:// などの URL。
type Source struct {
	Name     string
	Location string
}

// Render はマニフェストを text/template として評価する。テンプレート内で tfstate 関数が
// 実際に使われた state だけが (初回参照時に) 読み込まれる。
//
// state ごとに関数を登録する (ecspresso の func_prefix と同じ方式)。
// デフォルト state は {{ tfstate "addr" }} / {{ tfstatef "fmt" args }}、名前付き state は
// 名前をそのままプレフィックスにした {{ <name>tfstate "addr" }} / {{ <name>tfstatef ... }}。
func Render(ctx context.Context, manifest []byte, sources []Source) ([]byte, error) {
	funcs := template.FuncMap{
		"env":      envFunc,
		"must_env": mustEnvFunc,
	}

	for _, s := range sources {
		ldr := &stateLoader{ctx: ctx, loc: s.Location}
		// デフォルト state はプレフィックス無し、それ以外は名前をそのままプレフィックスに使う。
		prefix := ""
		if s.Name != DefaultStateName {
			prefix = s.Name
		}
		funcs[prefix+"tfstate"] = func(addr string) (string, error) {
			return ldr.lookup(addr)
		}
		funcs[prefix+"tfstatef"] = func(format string, args ...any) (string, error) {
			return ldr.lookup(fmt.Sprintf(format, args...))
		}
	}

	tmpl, err := template.New("manifest").Funcs(funcs).Parse(string(manifest))
	if err != nil {
		return nil, fmt.Errorf("failed to parse manifest template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, nil); err != nil {
		return nil, fmt.Errorf("failed to render manifest: %w", err)
	}
	return buf.Bytes(), nil
}

// envFunc は環境変数 name の値を返す (ecspresso 互換の {{ env "NAME" "default" }})。
// 未設定または空文字の場合は default を返す。default 省略時は空文字。
func envFunc(name string, def ...string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	if len(def) > 0 {
		return def[0]
	}
	return ""
}

// mustEnvFunc は環境変数 name の値を返す (ecspresso 互換の {{ must_env "NAME" }})。
// 変数が未定義の場合はエラー。空文字でも「定義済み」なら許容する。
func mustEnvFunc(name string) (string, error) {
	if v, ok := os.LookupEnv(name); ok {
		return v, nil
	}
	return "", fmt.Errorf("environment variable %q is not defined", name)
}

// stateLoader は state を遅延・一度きりで読み込み、属性を引く。
type stateLoader struct {
	ctx  context.Context
	loc  string
	once sync.Once
	st   *tfstate.TFState
	err  error
}

func (l *stateLoader) lookup(addr string) (string, error) {
	// ecspresso 互換: アドレス中の ' を " に置換し、YAML 内でのエスケープを不要にする
	// (例: aws_s3_bucket.main['id'] と書ける)。tfstate-lookup の nameFunc と同挙動。
	if strings.Contains(addr, "'") {
		addr = strings.ReplaceAll(addr, "'", "\"")
	}
	l.once.Do(func() {
		// スキーム付きは URL、そうでなければローカルファイルとして読む。
		if strings.Contains(l.loc, "://") {
			l.st, l.err = tfstate.ReadURL(l.ctx, l.loc)
		} else {
			l.st, l.err = tfstate.ReadFile(l.ctx, l.loc)
		}
	})
	if l.err != nil {
		return "", fmt.Errorf("failed to read tfstate %s: %w", l.loc, l.err)
	}

	obj, err := l.st.Lookup(addr)
	if err != nil {
		return "", fmt.Errorf("failed to look up %q in tfstate %s: %w", addr, l.loc, err)
	}
	if obj == nil || obj.Value == nil {
		return "", fmt.Errorf("%q not found in tfstate %s", addr, l.loc)
	}
	return obj.String(), nil
}
