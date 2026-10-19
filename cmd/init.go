package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/masasuzu/clrnd/internal/config"
	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"
)

// The name of the config file init generates. It matches the first of the root's auto-detected
// names (defaultConfigFiles).
const initConfigFile = "clrnd.yml"

// initOptions are init's flags. Each init command (services, jobs) has its own.
type initOptions struct {
	project  string
	region   string
	manifest string
	force    bool
}

var initCmd = newInitCmd(serviceTarget)

// newInitCmd builds init for a target.
func newInitCmd(t target) *cobra.Command {
	o := &initOptions{}
	format := "Knative-style YAML"
	if !t.isService() {
		format = "YAML"
	}
	cmd := &cobra.Command{
		Use:   fmt.Sprintf("init [%s]", t.noun),
		Short: fmt.Sprintf("Initialize a project from an existing %s", t.noun),
		Long: fmt.Sprintf("Fetch an existing Cloud Run %[1]s and scaffold a project from it: write its\n"+
			"manifest (%[2]s, with server-managed fields stripped) and a clrnd.yml\n"+
			"holding the project, region, %[1]s, and manifest path. Existing files are not\n"+
			"overwritten unless --force is given.\n"+
			"%[1]s may be omitted when set in the config file.", t.noun, format),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInit(cmd, args, t, o)
		},
		// Let -c choose where the config is generated. It is a file being generated, so it need not
		// exist yet.
		Annotations: map[string]string{annotationConfigOptional: ""},
	}
	if t.isService() {
		// init was called load once; the old name keeps working for services, which is all it
		// ever covered.
		cmd.Aliases = []string{"load"}
	}
	cmd.Flags().StringVarP(&o.manifest, "output", "o", "manifest.yaml", "manifest file to write")
	cmd.Flags().BoolVar(&o.force, "force", false, "overwrite existing files")
	addTargetFlags(cmd, &o.project, &o.region)
	return cmd
}

func runInit(cmd *cobra.Command, args []string, t target, o *initOptions) error {
	name, err := t.resolveName(args)
	if err != nil {
		return err
	}

	// When --config is given, write there. If the read location and the write location disagreed,
	// passing -c infra/clrnd.yml would still produce ./clrnd.yml. Without it, write to the config
	// that was found in the current directory, if any: a clrnd.yml written next to an existing
	// clrnd.yaml would be the one found next time, silently shadowing it.
	configFile := initConfigFile
	switch {
	case configPath != "":
		configFile = configPath
	case loadedConfig != "":
		configFile = loadedConfig
	}

	// To prevent accidental overwrites, check all the existing files up front, before writing any.
	manifestExisted := fileExists(o.manifest)
	if !o.force {
		for _, path := range []string{o.manifest, configFile} {
			if fileExists(path) {
				return fmt.Errorf("%s already exists: pass --force to overwrite", path)
			}
		}
	}

	ctx := cmd.Context()
	client, err := newCloudRunClient(cmd, o.project, o.region)
	if err != nil {
		return err
	}

	// A service loses its revision name here (see WithoutRevisionName).
	manifest, err := client.LiveManifest(ctx, t.kind, name)
	if err != nil {
		return err
	}

	// The manifest path written to the config is made relative to the config file.
	// resolveConfigPath resolves it against the config's directory, so recording it relative to
	// the cwd would break the path when -c points at another directory.
	configYAML, err := scaffoldConfig(t, client.Project(), client.Region(), name,
		manifestPathFor(configFile, o.manifest), keptTfstate(configFile))
	if err != nil {
		return err
	}

	// When --force is about to overwrite an existing manifest, keep its content so it can be
	// rolled back.
	var previousManifest []byte
	if manifestExisted {
		previousManifest, _ = os.ReadFile(o.manifest)
	}

	// The generated files can contain the live service's plaintext environment variables, so keep
	// them unreadable by other users. Without --force, creation and the existence check happen in
	// a single operation, so a file created after the check above is not overwritten. With
	// --force, the file is replaced atomically.
	if err := writeScaffold(o.manifest, manifest, o.force); err != nil {
		return err
	}
	if err := writeScaffold(configFile, configYAML, o.force); err != nil {
		// If writing the config fails, restore the manifest so no half-finished scaffold is left
		// behind. The rollback is best-effort, and the error returned is the original write error.
		restoreManifest(o.manifest, previousManifest, manifestExisted)
		return err
	}
	return nil
}

// writeScaffold writes one of init's generated files. With force it replaces an existing file
// atomically; otherwise it fails when the file already exists.
func writeScaffold(path string, data []byte, force bool) error {
	if force {
		return writeFilePrivate(path, data)
	}
	return writeFileExclusive(path, data)
}

// manifestPathFor returns the manifest's path relative to the config file.
// When it cannot be made relative (a different volume, etc.), the given path is used as-is.
func manifestPathFor(configFile, manifest string) string {
	// Both are made absolute first: filepath.Rel cannot relate an absolute path to a relative one
	// (init -c ~/infra/clrnd.yml, which the shell expands, with the default manifest.yaml), and
	// falling back to the cwd-relative path recorded a manifest the config would look for in its
	// own directory, where it is not.
	dir, err := filepath.Abs(filepath.Dir(configFile))
	if err != nil {
		return manifest
	}
	abs, err := filepath.Abs(manifest)
	if err != nil {
		return manifest
	}
	rel, err := filepath.Rel(dir, abs)
	if err != nil {
		// Different volumes on Windows: only an absolute path is right from anywhere.
		return abs
	}
	return rel
}

// keptTfstate returns the tfstate list of the config being overwritten, when init --force writes
// over the config it read. init scaffolds everything else a config holds, but not the states a
// templated manifest needs, so rebuilding the file from scratch dropped them, and the next render
// failed with "function tfstate not defined". The list is written back as it was read: its
// relative locations are relative to the same directory.
func keptTfstate(configFile string) []config.Tfstate {
	if loadedConfig == "" || len(cfg.Tfstate) == 0 {
		return nil
	}
	loaded, err1 := filepath.Abs(loadedConfig)
	target, err2 := filepath.Abs(configFile)
	if err1 != nil || err2 != nil || loaded != target {
		return nil
	}
	return cfg.Tfstate
}

// restoreManifest puts the manifest init rewrote back into its original state.
// If it did not exist before, it is removed; if it did, the saved content is written back.
func restoreManifest(path string, previous []byte, existed bool) {
	if !existed {
		_ = os.Remove(path)
		return
	}
	if previous != nil {
		_ = writeFilePrivate(path, previous)
	}
}

// fileExists reports whether a file (or directory) exists at path.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// scaffoldConfig builds the content of the clrnd.yml that init generates. Marshalling
// config.Config instead of writing it by hand leaves value escaping (when a path contains a colon,
// etc.) to the YAML library and keeps the schema in step with the reader of clrnd.yml
// (config.Load).
func scaffoldConfig(t target, project, region, name, manifest string, tfstate []config.Tfstate) ([]byte, error) {
	c := config.Config{Project: project, Region: region, Manifest: manifest, Tfstate: tfstate}
	if t.isService() {
		c.Service = name
	} else {
		c.Job = name
	}
	out, err := yaml.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("failed to build %s: %w", initConfigFile, err)
	}
	return out, nil
}
