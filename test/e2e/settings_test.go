//go:build e2e

package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type settings struct {
	project, region, root, workRoot, image, jobImage, only, oldRef string
	env                                                            []string
	keep, raw, cleanupOnly                                         bool
}

// Child processes own their environment and working directory. Do not use os.Chdir or
// os.Setenv here: later parallelization must not change another scenario's target.
func environment(base []string, overrides map[string]string) []string {
	out := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if _, ok := overrides[key]; !ok {
			out = append(out, entry)
		}
	}
	for k, v := range overrides {
		out = append(out, k+"="+v)
	}
	return out
}

func firstLine(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0]), nil
}

func readSettings() (settings, error) {
	_, file, _, _ := runtime.Caller(0)
	return readSettingsIn(filepath.Dir(file), valueOr("CLRND_E2E_CALLER_DIR", mustWorkingDirectory()))
}

func mustWorkingDirectory() string { dir, _ := os.Getwd(); return dir }

func absoluteFrom(base, path string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	return filepath.Abs(path)
}

func readSettingsIn(here, caller string) (settings, error) {
	c := settings{root: filepath.Clean(filepath.Join(here, "../..")),
		project: os.Getenv("PROJECT"), region: valueOr("REGION", "asia-northeast1"),
		image:    valueOr("IMAGE", "us-docker.pkg.dev/cloudrun/container/hello"),
		jobImage: valueOr("JOB_IMAGE", "us-docker.pkg.dev/cloudrun/container/job:latest"),
		only:     os.Getenv("ONLY"), oldRef: os.Getenv("OLD_REF"), env: os.Environ(),
		keep: os.Getenv("KEEP") == "1", raw: os.Getenv("RAW") == "1",
		cleanupOnly: os.Getenv("CLRND_E2E_CLEANUP_ORPHANS") == "1"}
	if root := os.Getenv("REPO"); root != "" {
		c.root = root
	}
	var err error
	if c.root, err = absoluteFrom(caller, c.root); err != nil {
		return c, err
	}
	switch c.only {
	case "", "current", "jobs", "old":
	default:
		return c, fmt.Errorf("unknown ONLY=%s: use current, jobs or old", c.only)
	}
	if c.only == "old" && c.oldRef == "" {
		return c, errors.New("ONLY=old requires OLD_REF")
	}
	if c.project == "" {
		c.project, err = firstLine(filepath.Join(here, "project.env"))
		if err != nil {
			return c, err
		}
	}
	if c.project == "" {
		return c, errors.New("no project configured: set PROJECT or test/e2e/project.env")
	}
	c.workRoot = valueOr("WORK_ROOT", os.TempDir())
	if c.workRoot, err = absoluteFrom(caller, c.workRoot); err != nil {
		return c, err
	}
	dir := os.Getenv("CLRND_E2E_GCLOUD_CONFIG")
	if dir == "" {
		dir, err = firstLine(filepath.Join(here, "gcloud-config.env"))
		if err != nil {
			return c, err
		}
		if dir != "" && !filepath.IsAbs(dir) && !strings.HasPrefix(dir, "~") {
			dir = filepath.Join(here, dir)
		}
	}
	if dir != "" {
		home, e := os.UserHomeDir()
		if e != nil {
			return c, e
		}
		if dir == "~" {
			dir = home
		} else if strings.HasPrefix(dir, "~/") {
			dir = filepath.Join(home, dir[2:])
		}
		dir, err = absoluteFrom(caller, dir)
		if err != nil {
			return c, err
		}
		adc := filepath.Join(dir, "application_default_credentials.json")
		b, e := os.ReadFile(adc)
		if e != nil {
			return c, fmt.Errorf("dedicated config has no readable ADC: %w", e)
		}
		var identity struct {
			Type string `json:"type"`
		}
		if e = json.Unmarshal(b, &identity); e != nil {
			return c, fmt.Errorf("invalid ADC: %w", e)
		}
		if identity.Type != "impersonated_service_account" && identity.Type != "service_account" {
			return c, fmt.Errorf("dedicated ADC is not a service account (%s): use --impersonate-service-account", identity.Type)
		}
		c.env = environment(c.env, map[string]string{"CLOUDSDK_CONFIG": dir, "GOOGLE_APPLICATION_CREDENTIALS": adc})
	}
	// A relative ADC supplied without a dedicated config must also survive cmd.Dir changes.
	for _, entry := range c.env {
		if v, ok := strings.CutPrefix(entry, "GOOGLE_APPLICATION_CREDENTIALS="); ok && v != "" {
			v, err = absoluteFrom(caller, v)
			if err != nil {
				return c, err
			}
			c.env = environment(c.env, map[string]string{"GOOGLE_APPLICATION_CREDENTIALS": v})
			break
		}
	}
	return c, nil
}
func valueOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
