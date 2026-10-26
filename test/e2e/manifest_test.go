//go:build e2e

package e2e

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func extractArchive(path, dest string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(filepath.FromSlash(h.Name))
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("archive path escapes destination: %s", h.Name)
		}
		target := filepath.Join(dest, name)
		switch h.Typeflag {
		case tar.TypeXGlobalHeader:
			// git archive records its commit hash here; this is metadata, not a file.
			continue
		case tar.TypeDir:
			if err = os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err = os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			out, e := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(h.Mode)&0o700)
			if e != nil {
				return e
			}
			_, e = io.Copy(out, tr)
			ce := out.Close()
			if e != nil {
				return e
			}
			if ce != nil {
				return ce
			}
		default:
			return fmt.Errorf("unsupported archive entry %s (type %d)", h.Name, h.Typeflag)
		}
	}
}

func serviceManifest(name, image string) []byte {
	return []byte(fmt.Sprintf("apiVersion: serving.knative.dev/v1\nkind: Service\nmetadata:\n  name: %s\nspec:\n  template:\n    spec:\n      containers:\n      - image: %s\n", name, image))
}
func jobManifest(name, image string) []byte {
	return []byte(fmt.Sprintf("apiVersion: run.googleapis.com/v1\nkind: Job\nmetadata:\n  name: %s\nspec:\n  template:\n    spec:\n      template:\n        spec:\n          maxRetries: 0\n          containers:\n          - image: %s\n", name, image))
}

func workerPoolManifest(name, image string) []byte {
	return []byte(fmt.Sprintf("apiVersion: run.googleapis.com/v1\nkind: WorkerPool\nmetadata:\n  name: %s\nspec:\n  template:\n    spec:\n      containers:\n      - image: %s\n", name, image))
}

// Edit parsed YAML rather than depending on key order or indentation in gcloud's output.
func (s *suite) edit(t *testing.T, path string, fn func(map[string]any)) {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal(s.read(t, path), &m); err != nil {
		s.fatal(t, "parse manifest: %v", err)
	}
	fn(m)
	b, err := yaml.Marshal(m)
	if err != nil {
		s.fatal(t, "encode manifest: %v", err)
	}
	s.write(t, path, b)
}
func mapping(m map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		v, ok := m[key].(map[string]any)
		if !ok {
			v = map[string]any{}
			m[key] = v
		}
		m = v
	}
	return m
}
func task(m map[string]any) map[string]any {
	if m["kind"] == "Job" {
		return mapping(m, "spec", "template", "spec", "template", "spec")
	}
	return mapping(m, "spec", "template", "spec")
}
func setEnv(m map[string]any, value string) {
	c := task(m)["containers"].([]any)[0].(map[string]any)
	entries, _ := c["env"].([]any)
	for _, entry := range entries {
		e := entry.(map[string]any)
		if e["name"] == "CLRND_E2E" {
			e["value"] = value
			return
		}
	}
	c["env"] = append(entries, map[string]any{"name": "CLRND_E2E", "value": value})
}

// setCommand overrides the first container's command; an empty one restores the image's own.
func setCommand(m map[string]any, command string) {
	c := task(m)["containers"].([]any)[0].(map[string]any)
	if command == "" {
		delete(c, "command")
		return
	}
	c["command"] = []any{command}
}

// setAnnotation sets one of the resource's own metadata.annotations.
func setAnnotation(m map[string]any, key, value string) {
	mapping(m, "metadata", "annotations")[key] = value
}
func pinRevision(m map[string]any, name string) {
	mapping(m, "spec", "template", "metadata")["name"] = name
}
