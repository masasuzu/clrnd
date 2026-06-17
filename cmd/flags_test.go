package cmd

import "testing"

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
