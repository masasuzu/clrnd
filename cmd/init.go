package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/masasuzu/clrnd/internal/cloudrun"
	"github.com/masasuzu/clrnd/internal/config"
	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"
)

// The name of the config file init generates. It matches the first of the root's auto-detected
// names (defaultConfigFiles).
const initConfigFile = "clrnd.yml"

var (
	initProject  string
	initRegion   string
	initManifest string
	initForce    bool
)

var initCmd = &cobra.Command{
	Use:     "init [service]",
	Aliases: []string{"load"},
	Short:   "Initialize a project from an existing service",
	Long: "Fetch an existing Cloud Run service and scaffold a project from it: write its\n" +
		"manifest (Knative-style YAML, with server-managed fields stripped) and a clrnd.yml\n" +
		"holding the project, region, service, and manifest path. Existing files are not\n" +
		"overwritten unless --force is given.\n" +
		"service may be omitted when set in the config file.",
	Args: cobra.MaximumNArgs(1),
	RunE: runInit,
	// Let -c choose where the config is generated. It is a file being generated, so it need not
	// exist yet.
	Annotations: map[string]string{annotationConfigOptional: ""},
}

func init() {
	addTargetFlags(initCmd, &initProject, &initRegion)
	initCmd.Flags().StringVarP(&initManifest, "output", "o", "manifest.yaml", "manifest file to write")
	initCmd.Flags().BoolVar(&initForce, "force", false, "overwrite existing files")
}

func runInit(cmd *cobra.Command, args []string) error {
	service, err := resolveService(args)
	if err != nil {
		return err
	}

	// When --config is given, write there. If the read location and the write location disagreed,
	// passing -c infra/clrnd.yml would still produce ./clrnd.yml.
	configFile := initConfigFile
	if configPath != "" {
		configFile = configPath
	}

	// To prevent accidental overwrites, check all the existing files up front, before writing any.
	manifestExisted := fileExists(initManifest)
	if !initForce {
		for _, path := range []string{initManifest, configFile} {
			if fileExists(path) {
				return fmt.Errorf("%s already exists: pass --force to overwrite", path)
			}
		}
	}

	ctx := cmd.Context()
	client, err := newCloudRunClient(cmd, initProject, initRegion)
	if err != nil {
		return err
	}

	obj, err := client.GetService(ctx, service)
	if err != nil {
		return err
	}
	// Keeping the live revision name as-is makes the second deploy that changes the template fail
	// with "a revision with the same name and a different configuration cannot be created". The
	// scaffold drops it and leaves naming to Cloud Run's automatic numbering.
	manifest, err := cloudrun.ToManifest(cloudrun.WithoutRevisionName(obj))
	if err != nil {
		return err
	}

	// The manifest path written to the config is made relative to the config file.
	// resolveConfigPath resolves it against the config's directory, so recording it relative to
	// the cwd would break the path when -c points at another directory.
	configYAML, err := scaffoldConfig(client.Project(), client.Region(), service,
		manifestPathFor(configFile, initManifest))
	if err != nil {
		return err
	}

	// When --force is about to overwrite an existing manifest, keep its content so it can be
	// rolled back.
	var previousManifest []byte
	if manifestExisted {
		previousManifest, _ = os.ReadFile(initManifest)
	}

	// The generated files can contain the live service's plaintext environment variables, so keep
	// them unreadable by other users. Without --force, creation and the existence check happen in
	// a single operation, so a file created after the check above is not overwritten. With
	// --force, the file is replaced atomically.
	if err := writeScaffold(initManifest, manifest, initForce); err != nil {
		return err
	}
	if err := writeScaffold(configFile, configYAML, initForce); err != nil {
		// If writing the config fails, restore the manifest so no half-finished scaffold is left
		// behind. The rollback is best-effort, and the error returned is the original write error.
		restoreManifest(initManifest, previousManifest, manifestExisted)
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
	rel, err := filepath.Rel(filepath.Dir(configFile), manifest)
	if err != nil {
		return manifest
	}
	return rel
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
func scaffoldConfig(project, region, service, manifest string) ([]byte, error) {
	out, err := yaml.Marshal(config.Config{
		Project:  project,
		Region:   region,
		Service:  service,
		Manifest: manifest,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to build %s: %w", initConfigFile, err)
	}
	return out, nil
}
