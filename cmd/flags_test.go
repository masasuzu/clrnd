package cmd

import (
	"testing"

	"github.com/masasuzu/clrnd/internal/config"
)

// withConfig は cfg を一時的に差し替え、テスト後に元へ戻す。
func withConfig(t *testing.T, c *config.Config) {
	t.Helper()
	prev := cfg
	cfg = c
	t.Cleanup(func() { cfg = prev })
}

// withConfigDir は configDir を一時的に差し替え、テスト後に元へ戻す。
func withConfigDir(t *testing.T, dir string) {
	t.Helper()
	prev := configDir
	configDir = dir
	t.Cleanup(func() { configDir = prev })
}

func TestResolveConfigPath(t *testing.T) {
	withConfigDir(t, "/etc/app")
	cases := []struct {
		in, want string
	}{
		{"manifest.yaml", "/etc/app/manifest.yaml"}, // 相対 → config dir 基準
		{"sub/m.yaml", "/etc/app/sub/m.yaml"},
		{"/abs/m.yaml", "/abs/m.yaml"},                                  // 絶対はそのまま
		{"gs://bucket/state.tfstate", "gs://bucket/state.tfstate"},      // URL はそのまま
		{"", ""},
	}
	for _, c := range cases {
		if got := resolveConfigPath(c.in); got != c.want {
			t.Errorf("resolveConfigPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	t.Run("empty configDir leaves relative path as-is", func(t *testing.T) {
		withConfigDir(t, "")
		if got := resolveConfigPath("manifest.yaml"); got != "manifest.yaml" {
			t.Errorf("got %q, want manifest.yaml", got)
		}
	})
}

func TestResolveManifestUsesConfigDir(t *testing.T) {
	withConfig(t, &config.Config{Manifest: "manifest.yaml"})
	withConfigDir(t, "/etc/app")

	t.Run("config manifest resolved against config dir", func(t *testing.T) {
		got, err := resolveManifest(nil)
		if err != nil || got != "/etc/app/manifest.yaml" {
			t.Fatalf("resolveManifest(nil) = %q, %v; want /etc/app/manifest.yaml", got, err)
		}
	})

	t.Run("positional arg stays cwd-relative", func(t *testing.T) {
		got, err := resolveManifest([]string{"svc", "arg.yaml"})
		if err != nil || got != "arg.yaml" {
			t.Fatalf("resolveManifest(args) = %q, %v; want arg.yaml", got, err)
		}
	})
}

func TestResolveUsesConfigFallback(t *testing.T) {
	t.Run("config project used when flag and env empty", func(t *testing.T) {
		t.Setenv(envProjectPrimary, "")
		t.Setenv(envProjectSecondary, "")
		withConfig(t, &config.Config{Project: "cfg-project", Region: "cfg-region"})

		got, err := resolveProject("")
		if err != nil || got != "cfg-project" {
			t.Fatalf("resolveProject() = %q, %v; want cfg-project, nil", got, err)
		}
	})

	t.Run("env wins over config", func(t *testing.T) {
		t.Setenv(envProjectPrimary, "env-project")
		withConfig(t, &config.Config{Project: "cfg-project"})

		got, err := resolveProject("")
		if err != nil || got != "env-project" {
			t.Fatalf("resolveProject() = %q, %v; want env-project, nil", got, err)
		}
	})

	t.Run("flag wins over config and env", func(t *testing.T) {
		t.Setenv(envRegionPrimary, "env-region")
		withConfig(t, &config.Config{Region: "cfg-region"})

		got, err := resolveRegion("flag-region")
		if err != nil || got != "flag-region" {
			t.Fatalf("resolveRegion() = %q, %v; want flag-region, nil", got, err)
		}
	})
}

func TestResolveServiceAndManifest(t *testing.T) {
	t.Run("args take precedence over config", func(t *testing.T) {
		withConfig(t, &config.Config{Service: "cfg-svc", Manifest: "cfg.yaml"})
		svc, err := resolveService([]string{"arg-svc", "arg.yaml"})
		if err != nil || svc != "arg-svc" {
			t.Fatalf("resolveService() = %q, %v", svc, err)
		}
		m, err := resolveManifest([]string{"arg-svc", "arg.yaml"})
		if err != nil || m != "arg.yaml" {
			t.Fatalf("resolveManifest() = %q, %v", m, err)
		}
	})

	t.Run("config used when args absent", func(t *testing.T) {
		withConfig(t, &config.Config{Service: "cfg-svc", Manifest: "cfg.yaml"})
		svc, err := resolveService(nil)
		if err != nil || svc != "cfg-svc" {
			t.Fatalf("resolveService() = %q, %v", svc, err)
		}
		m, err := resolveManifest(nil)
		if err != nil || m != "cfg.yaml" {
			t.Fatalf("resolveManifest() = %q, %v", m, err)
		}
	})

	t.Run("single arg fills service, manifest from config", func(t *testing.T) {
		withConfig(t, &config.Config{Manifest: "cfg.yaml"})
		svc, err := resolveService([]string{"arg-svc"})
		if err != nil || svc != "arg-svc" {
			t.Fatalf("resolveService() = %q, %v", svc, err)
		}
		m, err := resolveManifest([]string{"arg-svc"})
		if err != nil || m != "cfg.yaml" {
			t.Fatalf("resolveManifest() = %q, %v", m, err)
		}
	})

	t.Run("error when neither arg nor config", func(t *testing.T) {
		withConfig(t, &config.Config{})
		if _, err := resolveService(nil); err == nil {
			t.Error("resolveService() = nil error, want error")
		}
		if _, err := resolveManifest(nil); err == nil {
			t.Error("resolveManifest() = nil error, want error")
		}
	})
}

func TestResolveTfstateSources(t *testing.T) {
	t.Run("flag overrides config", func(t *testing.T) {
		withConfig(t, &config.Config{Tfstate: []config.Tfstate{{Location: "gs://cfg/state"}}})
		got, err := resolveTfstateSources([]string{"net=gs://flag/state"})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(got) != 1 || got[0].Name != "net" || got[0].Location != "gs://flag/state" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("config used when no flag", func(t *testing.T) {
		withConfig(t, &config.Config{Tfstate: []config.Tfstate{
			{Location: "gs://cfg/app"},
			{Name: "network", Location: "gs://cfg/net"},
		}})
		got, err := resolveTfstateSources(nil)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(got) != 2 || got[0].Name != "default" || got[1].Name != "network" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("config tfstate without location errors", func(t *testing.T) {
		withConfig(t, &config.Config{Tfstate: []config.Tfstate{{Name: "x"}}})
		if _, err := resolveTfstateSources(nil); err == nil {
			t.Fatal("want error for missing location")
		}
	})
}

func TestParseTfstateSources(t *testing.T) {
	t.Run("location only uses default name", func(t *testing.T) {
		got, err := parseTfstateSources([]string{"terraform.tfstate"})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(got) != 1 || got[0].Name != "default" || got[0].Location != "terraform.tfstate" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("name=location", func(t *testing.T) {
		got, err := parseTfstateSources([]string{"network=gs://bucket/net.tfstate"})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if got[0].Name != "network" || got[0].Location != "gs://bucket/net.tfstate" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("url with = in query is treated as location", func(t *testing.T) {
		got, err := parseTfstateSources([]string{"gs://bucket/path?generation=123"})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if got[0].Name != "default" || got[0].Location != "gs://bucket/path?generation=123" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("duplicate name errors", func(t *testing.T) {
		_, err := parseTfstateSources([]string{"a=x", "a=y"})
		if err == nil {
			t.Fatal("want duplicate error")
		}
	})

	t.Run("empty location errors", func(t *testing.T) {
		_, err := parseTfstateSources([]string{"name="})
		if err == nil {
			t.Fatal("want empty location error")
		}
	})
}

func TestResolveProject(t *testing.T) {
	t.Run("flag wins over env", func(t *testing.T) {
		t.Setenv(envProjectPrimary, "env-primary")
		t.Setenv(envProjectSecondary, "env-secondary")
		got, err := resolveProject("flag-value")
		if err != nil || got != "flag-value" {
			t.Fatalf("resolveProject() = %q, %v; want flag-value, nil", got, err)
		}
	})

	t.Run("primary env used when flag empty", func(t *testing.T) {
		t.Setenv(envProjectPrimary, "env-primary")
		t.Setenv(envProjectSecondary, "env-secondary")
		got, err := resolveProject("")
		if err != nil || got != "env-primary" {
			t.Fatalf("resolveProject() = %q, %v; want env-primary, nil", got, err)
		}
	})

	t.Run("secondary env used when primary empty", func(t *testing.T) {
		t.Setenv(envProjectPrimary, "")
		t.Setenv(envProjectSecondary, "env-secondary")
		got, err := resolveProject("")
		if err != nil || got != "env-secondary" {
			t.Fatalf("resolveProject() = %q, %v; want env-secondary, nil", got, err)
		}
	})

	t.Run("error when nothing set", func(t *testing.T) {
		t.Setenv(envProjectPrimary, "")
		t.Setenv(envProjectSecondary, "")
		if _, err := resolveProject(""); err == nil {
			t.Fatal("resolveProject() = nil error; want error")
		}
	})

	t.Run("whitespace-only env falls through to secondary", func(t *testing.T) {
		t.Setenv(envProjectPrimary, "   ")
		t.Setenv(envProjectSecondary, "env-secondary")
		got, err := resolveProject("")
		if err != nil || got != "env-secondary" {
			t.Fatalf("resolveProject() = %q, %v; want env-secondary, nil", got, err)
		}
	})

	t.Run("surrounding whitespace is trimmed", func(t *testing.T) {
		t.Setenv(envProjectPrimary, "  my-project  ")
		t.Setenv(envProjectSecondary, "")
		got, err := resolveProject("")
		if err != nil || got != "my-project" {
			t.Fatalf("resolveProject() = %q, %v; want my-project, nil", got, err)
		}
	})
}

func TestResolveRegion(t *testing.T) {
	t.Run("flag wins over env", func(t *testing.T) {
		t.Setenv(envRegionPrimary, "env-primary")
		got, err := resolveRegion("flag-value")
		if err != nil || got != "flag-value" {
			t.Fatalf("resolveRegion() = %q, %v; want flag-value, nil", got, err)
		}
	})

	t.Run("primary env used when flag empty", func(t *testing.T) {
		t.Setenv(envRegionPrimary, "asia-northeast1")
		t.Setenv(envRegionSecondary, "")
		got, err := resolveRegion("")
		if err != nil || got != "asia-northeast1" {
			t.Fatalf("resolveRegion() = %q, %v; want asia-northeast1, nil", got, err)
		}
	})

	t.Run("error when nothing set", func(t *testing.T) {
		t.Setenv(envRegionPrimary, "")
		t.Setenv(envRegionSecondary, "")
		if _, err := resolveRegion(""); err == nil {
			t.Fatal("resolveRegion() = nil error; want error")
		}
	})
}
