package commands

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"os/exec"

	"github.com/openfaas/faas-cli/stack"
	"github.com/spf13/cobra"
)

const localSecretsDir = ".secrets"

func init() {
	faasCmd.AddCommand(newLocalRunCmd())

}

type runOptions struct {
	print    bool
	port     int
	network  string
	extraEnv map[string]string
	output   io.Writer
	err      io.Writer
}

func newLocalRunCmd() *cobra.Command {
	opts := runOptions{}

	cmd := &cobra.Command{
		Use:   `local-run NAME --port PORT -f YAML_FILE`,
		Short: "Start a function with docker for local testing (experimental feature)",
		Long: `Providing faas-cli build has already been run, this command will use the
docker command to start a container on your local machine using its image.

The function will be bound to the port specified by the --port flag, or 8080
by default.

There is limited support for secrets, and the function cannot contact other
services deployed within your OpenFaaS cluster.`,
		Example: `
  # Run a function locally
  faas-cli local-run stronghash

  # Run on a custom port
  faas-cli local-run stronghash --port 8081

  # Use a custom YAML file other than stack.yml
  faas-cli local-run stronghash -f ./stronghash.yml
		`,
		PreRunE: func(cmd *cobra.Command, args []string) error {

			if v, ok := os.LookupEnv("OPENFAAS_EXPERIMENTAL"); !ok || v == "0" {
				return fmt.Errorf("this command is experimental, set OPENFAAS_EXPERIMENTAL=1 to use it")
			}

			if len(args) < 1 {
				return fmt.Errorf("expected the name of the function")
			}

			if len(args) > 1 {
				return fmt.Errorf("only one function name is allowed")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			opts.output = cmd.OutOrStdout()
			opts.err = cmd.ErrOrStderr()

			return runFunction(ctx, args[0], opts)
		},
		// TODO: unhide once we are happy with the DX.
		Hidden: true,
	}

	cmd.Flags().BoolVar(&opts.print, "print", false, "Print the docker command instead of running it")
	cmd.Flags().IntVarP(&opts.port, "port", "p", 8080, "port to bind the function to")
	cmd.Flags().StringVar(&opts.network, "network", "", "connect function to an existing network, use 'host' to access other process already running on localhost. When using this, '--port' is ignored, if you have port collisions, you may change the port using '-e port=NEW_PORT'")
	cmd.Flags().StringToStringVarP(&opts.extraEnv, "env", "e", map[string]string{}, "additional environment variables (ENVVAR=VALUE), use this to experiment with different values for your function")

	return cmd
}

func runFunction(ctx context.Context, name string, opts runOptions) error {
	services, err := stack.ParseYAMLFile(yamlFile, "", name, true)
	if err != nil {
		return err
	}

	if len(services.Functions) > 1 {
		return fmt.Errorf("multiple functions matching %q in the stack file", name)
	}

	err = updateGitignore()
	if err != nil {
		return err
	}

	fnc := services.Functions[name]
	// TODO: we should probably use a levelled logger here
	// fmt.Fprintf(opts.output, "%#v\n\n", fnc)

	cmd, err := buildDockerRun(ctx, fnc, opts)
	if err != nil {
		return err
	}

	if opts.print {
		fmt.Fprintf(opts.output, "%s\n", cmd.String())
		return nil
	}

	cmd.Stdout = opts.output
	cmd.Stderr = opts.err

	fmt.Printf("Starting local-run for: %s on: http://0.0.0.0:%d\n\n", name, opts.port)

	if err = cmd.Start(); err != nil {
		return err
	}

	return cmd.Wait()
}

// buildDockerRun constructs a exec.Cmd from the given stack Function
func buildDockerRun(ctx context.Context, fnc stack.Function, opts runOptions) (*exec.Cmd, error) {
	args := []string{"run", "--rm", "-i", fmt.Sprintf("-p=%d:8080", opts.port)}

	if opts.network != "" {
		args = append(args, fmt.Sprintf("--network=%s", opts.network))
	}

	fprocess, err := deriveFprocess(fnc)
	if err != nil {
		return nil, err
	}

	for name, value := range fnc.Environment {
		args = append(args, fmt.Sprintf("-e=%s=%s", name, value))
	}

	moreEnv, err := readFiles(fnc.EnvironmentFile)
	if err != nil {
		return nil, err
	}

	for name, value := range moreEnv {
		args = append(args, fmt.Sprintf("-e=%s=%s", name, value))
	}

	for name, value := range opts.extraEnv {
		args = append(args, fmt.Sprintf("-e=%s=%s", name, value))
	}

	if fnc.ReadOnlyRootFilesystem {
		args = append(args, "--read-only")
	}

	if fnc.Limits != nil {
		if fnc.Limits.Memory != "" {
			// use a soft limit for debugging
			args = append(args, fmt.Sprintf("--memory-reservation=%s", fnc.Limits.Memory))
		}

		if fnc.Limits.CPU != "" {
			args = append(args, fmt.Sprintf("--cpus=%s", fnc.Limits.CPU))
		}
	}

	if len(fnc.Secrets) > 0 {
		secretsPath, err := filepath.Abs(localSecretsDir)
		if err != nil {
			return nil, fmt.Errorf("can't determine secrets folder: %w", err)
		}

		err = os.MkdirAll(secretsPath, 0700)
		if err != nil {
			return nil, fmt.Errorf("can't create local secrets folder %q: %w", secretsPath, err)
		}

		if !opts.print {
			err = dirContainsFiles(secretsPath, fnc.Secrets...)
			if err != nil {
				return nil, fmt.Errorf("missing files: %w", err)
			}
		}

		args = append(args, fmt.Sprintf("--volume=%s:/var/openfaas/secrets", secretsPath))
	}

	args = append(args, fmt.Sprintf("-e=fprocess=%s", fprocess))
	args = append(args, fnc.Image)

	cmd := exec.CommandContext(ctx, "docker", args...)

	return cmd, nil
}

func dirContainsFiles(dir string, names ...string) error {
	var err = &missingFileError{
		dir:     dir,
		missing: []string{},
	}

	for _, name := range names {
		path := filepath.Join(dir, name)
		_, statErr := os.Stat(path)
		if statErr != nil {
			err.missing = append(err.missing, name)
		}
	}

	if len(err.missing) > 0 {
		return err
	}

	return nil
}

type missingFileError struct {
	missing []string
	dir     string
}

func (m missingFileError) Error() string {
	return fmt.Sprintf("create the following secrets (%s) in: %q", strings.Join(m.missing, ", "), m.dir)
}

func (m *missingFileError) AddMissingSecret(p string) {
	m.missing = append(m.missing, p)
}
