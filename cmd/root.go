package cmd

import (
	"os"

	"github.com/airbytehq/airbyte-agent-cli/internal/registry"
	"github.com/spf13/cobra"
)

var (
	output        string
	verbose       bool
	fields        []string
	executionMode string
	awsProfile    string
	awsRegion     string
)

var rootCmd = &cobra.Command{
	Use:   "airbyte-agent",
	Short: "Airbyte Agent CLI",
	Long:  "Command-line interface for interacting with the Airbyte platform.",
	Args:  registry.UnknownSubcommandArgs,
	Run: func(cmd *cobra.Command, args []string) {
		printSplash(os.Stdout)
	},
	SilenceUsage: true,
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&output, "output", "o", "", "Output file path")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable debug logging")
	rootCmd.PersistentFlags().StringSliceVar(&fields, "fields", nil, "Filter response to only the listed fields (comma-separated, dotted paths, e.g. 'data.id,data.name')")

	// Runtime provider controls. These are persistent ROOT flags (not
	// per-operation params) so they compose with --json and other output
	// flags. They carry no secret material.
	rootCmd.PersistentFlags().StringVar(&executionMode, "execution-mode", "", "Connector execution mode: hosted (default) or local")
	rootCmd.PersistentFlags().StringVar(&awsProfile, "aws-profile", "", "AWS shared-config profile for secret hydration (local mode); authoritative when set")
	rootCmd.PersistentFlags().StringVar(&awsRegion, "aws-region", "", "AWS region for secret hydration (local mode)")
}

func Execute() error {
	return rootCmd.Execute()
}

func GetRootCmd() *cobra.Command {
	return rootCmd
}

func GetVerbose() bool {
	return verbose
}

func GetOutput() string {
	return output
}

// GetExecutionMode returns the raw --execution-mode flag value ("" if unset).
// Resolution of precedence and validation happens in config.ResolveExecutionConfig.
func GetExecutionMode() string {
	return executionMode
}

// GetAWSProfile returns the raw --aws-profile flag value ("" if unset).
func GetAWSProfile() string {
	return awsProfile
}

// GetAWSRegion returns the raw --aws-region flag value ("" if unset).
func GetAWSRegion() string {
	return awsRegion
}

type flags struct{}

func (f flags) GetOutput() string   { return output }
func (f flags) GetFields() []string { return fields }

func FlagAccessor() flags {
	return flags{}
}
