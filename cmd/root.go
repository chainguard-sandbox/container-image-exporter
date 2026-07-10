package cmd

import (
	"os"

	"github.com/prometheus/common/version"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:     "container-image-exporter",
	Short:   "Export Prometheus metrics about container images in a Kubernetes cluster.",
	Version: version.Print("container-image-exporter"),
}

// Execute runs the root command.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	rootCmd.SetVersionTemplate("{{.Version}}\n")
	rootCmd.AddCommand(clusterExporterCmd)
	rootCmd.AddCommand(nodeExporterCmd)
}
