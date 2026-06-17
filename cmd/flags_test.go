package cmd

import "testing"

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
