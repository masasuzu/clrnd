package cloudrun

import (
	"fmt"
	"strings"

	run "google.golang.org/api/run/v1"
	"sigs.k8s.io/yaml"
)

// ImageOverride は --image の指定 1 件。Container が空なら「マニフェストに 1 つしかない
// コンテナ」を指す。
type ImageOverride struct {
	Container string
	Image     string
}

// ParseImageOverride は --image の値を解釈する。形は "<image>" か "<container>=<image>"。
//
// 最初の "=" で切って曖昧さは無い: コンテナ名にもイメージ参照 (host/path:tag@digest) にも
// "=" は現れない。
func ParseImageOverride(spec string) (ImageOverride, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return ImageOverride{}, fmt.Errorf("--image needs a value: <image> or <container>=<image>")
	}
	name, image := "", spec
	if i := strings.Index(spec, "="); i >= 0 {
		name, image = strings.TrimSpace(spec[:i]), strings.TrimSpace(spec[i+1:])
	}
	if image == "" {
		return ImageOverride{}, fmt.Errorf("--image %q has no image", spec)
	}
	if strings.Contains(spec, "=") && name == "" {
		return ImageOverride{}, fmt.Errorf("--image %q has no container name before the '='", spec)
	}
	return ImageOverride{Container: name, Image: image}, nil
}

// ApplyImageOverrides はマニフェストの containers[].image を差し替えたものを返す。
// 指定が無ければマニフェストをそのまま返す (指定が無いときに整形し直さないため)。
//
// 差し替えは「マニフェストが唯一の入力」という原則の例外なので、当てられる範囲を狭く
// 保つ: 名前を省略できるのはコンテナが 1 つのときだけで、複数あるサービスでは
// どのコンテナかを明示させる。黙って先頭を書き換えると、サイドカーを持つサービスで
// 意図しないコンテナが差し替わる。
func ApplyImageOverrides(manifest []byte, specs []string) ([]byte, error) {
	if len(specs) == 0 {
		return manifest, nil
	}
	overrides := make([]ImageOverride, 0, len(specs))
	for _, spec := range specs {
		o, err := ParseImageOverride(spec)
		if err != nil {
			return nil, err
		}
		overrides = append(overrides, o)
	}

	svc, err := parseManifest(manifest)
	if err != nil {
		return nil, err
	}
	containers := serviceContainers(svc)
	if len(containers) == 0 {
		return nil, fmt.Errorf("--image needs a container to apply to, but the manifest defines none")
	}

	for _, o := range overrides {
		target, err := findContainer(containers, o.Container)
		if err != nil {
			return nil, err
		}
		target.Image = o.Image
	}

	out, err := yaml.Marshal(svc)
	if err != nil {
		return nil, fmt.Errorf("failed to rebuild the manifest after --image: %w", err)
	}
	return out, nil
}

// findContainer は名前でコンテナを探す。name が空ならコンテナが 1 つの場合だけ成功する。
func findContainer(containers []*run.Container, name string) (*run.Container, error) {
	if name == "" {
		if len(containers) != 1 {
			return nil, fmt.Errorf(
				"--image needs a container name because the manifest defines %d containers (%s); "+
					"use --image <container>=<image>", len(containers), strings.Join(containerNames(containers), ", "))
		}
		return containers[0], nil
	}
	for _, c := range containers {
		if c != nil && c.Name == name {
			return c, nil
		}
	}
	return nil, fmt.Errorf("--image names container %q, which the manifest does not define (%s)",
		name, strings.Join(containerNames(containers), ", "))
}

// containerNames はエラーに載せるコンテナ名の一覧。名前の無いコンテナは "<unnamed>"。
func containerNames(containers []*run.Container) []string {
	out := make([]string, 0, len(containers))
	for _, c := range containers {
		switch {
		case c == nil:
			continue
		case c.Name == "":
			out = append(out, "<unnamed>")
		default:
			out = append(out, c.Name)
		}
	}
	return out
}
