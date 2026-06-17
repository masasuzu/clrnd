// Package config は clrnd の設定ファイル (YAML) を読み込む。
// フラグや環境変数が未指定のときのフォールバック値を提供する。
package config

import (
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

// Config は設定ファイルの内容。フィールドは sigs.k8s.io/yaml が JSON タグで解釈する。
type Config struct {
	Project  string    `json:"project"`
	Region   string    `json:"region"`
	Service  string    `json:"service"`
	Manifest string    `json:"manifest"`
	Tfstate  []Tfstate `json:"tfstate"`
}

// Tfstate は名前付き Terraform state の宣言。Name 省略時は "default" 扱い。
type Tfstate struct {
	Name     string `json:"name"`
	Location string `json:"location"`
}

// Load は path の設定ファイルを厳密に読み込む。未知キー (打ち間違い) も検出する。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config %s: %w", path, err)
	}
	var c Config
	if err := yaml.UnmarshalStrict(data, &c); err != nil {
		return nil, fmt.Errorf("failed to parse config %s: %w", path, err)
	}
	return &c, nil
}
