// Package config reads clrnd's config file (YAML).
// It supplies the fallback values used when a flag is not given.
package config

import (
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

// Config is the content of the config file. sigs.k8s.io/yaml interprets its fields through their
// JSON tags. omitempty has no effect on reading (UnmarshalStrict); it is there so that empty fields
// are left out when init marshals a Config to generate clrnd.yml.
type Config struct {
	Project string `json:"project,omitempty"`
	Region  string `json:"region,omitempty"`
	Service string `json:"service,omitempty"`
	// Job names a Cloud Run job instead of a service. A config file describes one deployable
	// resource, so at most one of Service and Job is set (Load enforces it). Keeping a key per kind
	// rather than one shared "name" means a command for the other kind finds nothing to act on,
	// instead of acting on a resource of its own kind that happens to share the name.
	Job      string    `json:"job,omitempty"`
	Manifest string    `json:"manifest,omitempty"`
	Tfstate  []Tfstate `json:"tfstate,omitempty"`
}

// Tfstate declares a named Terraform state. An omitted Name is treated as "default".
type Tfstate struct {
	Name     string `json:"name"`
	Location string `json:"location"`
}

// Load reads the config file at path strictly. It also detects unknown keys (typos).
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config %s: %w", path, err)
	}
	var c Config
	if err := yaml.UnmarshalStrict(data, &c); err != nil {
		return nil, fmt.Errorf("failed to parse config %s: %w", path, err)
	}
	if c.Service != "" && c.Job != "" {
		return nil, fmt.Errorf("config %s sets both service and job: a config file describes one of them", path)
	}
	return &c, nil
}
