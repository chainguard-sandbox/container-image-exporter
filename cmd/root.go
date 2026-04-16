package cmd

import (
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "container-image-exporter",
	Short: "Export Prometheus metrics about container images in a Kubernetes cluster.",
}

// Execute runs the root command.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	rootCmd.AddCommand(exporterCmd)
	rootCmd.AddCommand(nodeExporterCmd)
}
