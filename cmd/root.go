package cmd

import (
	"os"

	"github.com/airbytehq/airbyte-agent-cli/internal/registry"
	"github.com/spf13/cobra"
)

var (
	output  string
	verbose bool
	fields  []string
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
	rootCmd.PersistentFlags().StringSliceVar(&fields, "fields", nil, "Filter response to listed dotted paths; fails if none match (e.g. 'data.id,data.name')")
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

type flags struct{}

func (f flags) GetOutput() string   { return output }
func (f flags) GetFields() []string { return fields }

func FlagAccessor() flags {
	return flags{}
}
