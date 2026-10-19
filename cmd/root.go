package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/masasuzu/clrnd/internal/cloudrun"
	"github.com/masasuzu/clrnd/internal/config"
	"github.com/spf13/cobra"
	"google.golang.org/api/googleapi"
)

// Config file names looked for when none is given (in the current directory).
var defaultConfigFiles = []string{"clrnd.yml", "clrnd.yaml"}

var (
	configPath string
	// cfg is the loaded config. Empty when none is given (nil-safe).
	cfg = &config.Config{}
	// configDir is the directory of the loaded config file, the base for relative paths from the
	// config.
	configDir string
	// loadedConfig is the path of the config file that was read, whether given with --config or
	// found in the current directory. Empty when none was.
	loadedConfig string
)

var rootCmd = &cobra.Command{
	Use:               "clrnd",
	Short:             "A CLI for deploying to Cloud Run",
	PersistentPreRunE: loadConfig,
}

// Execute runs the root command. It passes a context that SIGINT/SIGTERM cancels, so each
// subcommand can be interrupted with Ctrl-C by using cmd.Context().
func Execute() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Once the first signal has cancelled ctx, release the handler and return to the default
	// behaviour (exit immediately). Without that, every later signal is swallowed, and while the
	// process is in code that does not watch ctx there is no way left to stop it.
	go func() {
		<-ctx.Done()
		stop()
	}()
	err := rootCmd.ExecuteContext(ctx)
	if err != nil {
		// Printed here rather than by cobra (SilenceErrors), so the message can be fixed up first.
		fmt.Fprintln(rootCmd.ErrOrStderr(), "Error:", errorMessage(err))
	}
	return err
}

// errorMessage is what Execute prints for err: its own text, except that a 404 which did not come
// from the API (cloudrun.EndpointNotFound) has the web page it carries replaced with what it most
// likely means. Left as is, a region Cloud Run does not have printed a page of HTML.
func errorMessage(err error) string {
	msg := err.Error()
	var gerr *googleapi.Error
	if !cloudrun.EndpointNotFound(err) || !errors.As(err, &gerr) {
		return msg
	}
	return strings.Replace(msg, gerr.Error(), "the Cloud Run endpoint for this region answered 404 with a web "+
		"page instead of an API error; check that the region (--region or region in the config file) "+
		"is one Cloud Run has", 1)
}

func init() {
	// Do not print the whole usage on every runtime error. The carefully built error message gets
	// buried under a flag list dozens of lines long, which hurts most on Ctrl-C or a failed
	// rollout.
	//
	// SilenceUsage also applies to flag and argument parse errors, so those get a one-line hint
	// instead. A short pointer to where to look is more useful than the full usage.
	rootCmd.SilenceUsage = true
	rootCmd.SilenceErrors = true
	rootCmd.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return fmt.Errorf("%w\nRun '%s --help' for usage", err, c.CommandPath())
	})
	rootCmd.Version = buildVersion()
	rootCmd.SetVersionTemplate("{{ .Name }} version {{ .Version }}\n")
	rootCmd.PersistentFlags().StringVarP(&configPath, "config", "c", "",
		"config file (default: clrnd.yml or clrnd.yaml in the current directory)")
	rootCmd.AddCommand(verifyCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(revisionsCmd)
	rootCmd.AddCommand(rollbackCmd)
	rootCmd.AddCommand(trafficCmd)
	rootCmd.AddCommand(deleteCmd)
	rootCmd.AddCommand(refreshCmd)
	rootCmd.AddCommand(waitCmd)
	rootCmd.AddCommand(renderCmd)
	rootCmd.AddCommand(diffCmd)
	rootCmd.AddCommand(deployCmd)
	rootCmd.AddCommand(initCmd)
	rootCmd.AddCommand(jobsCmd)
}

// A subcommand carrying annotationConfigOptional does not fail when the file given explicitly with
// --config does not exist. It is for a command that "creates" the config file rather than "reads"
// it (init).
const annotationConfigOptional = "clrnd/config-optional"

// loadConfig loads the config file named by --config, or one with a default name when it is not
// given. A missing file is an error when --config is explicit; when auto-detecting, a missing
// file means nothing is done.
func loadConfig(cmd *cobra.Command, args []string) error {
	path := configPath
	if path == "" {
		path = findDefaultConfig()
		if path == "" {
			return nil
		}
	} else if _, ok := cmd.Annotations[annotationConfigOptional]; ok {
		// init is the side that generates clrnd.yml, so it is normal for the destination given
		// with -c not to exist yet. Read it only when it exists, and carry its values over.
		if !fileExists(path) {
			return nil
		}
	}
	c, err := config.Load(path)
	if err != nil {
		return err
	}
	cfg = c
	configDir = filepath.Dir(path)
	loadedConfig = path
	return nil
}

func findDefaultConfig() string {
	for _, name := range defaultConfigFiles {
		if info, err := os.Stat(name); err == nil && !info.IsDir() {
			return name
		}
	}
	return ""
}
