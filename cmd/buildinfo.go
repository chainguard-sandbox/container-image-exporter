package cmd

import (
	"github.com/prometheus/client_golang/prometheus"
	collectorsversion "github.com/prometheus/client_golang/prometheus/collectors/version"
)

func newBuildInfoCollector(program string) prometheus.Collector {
	return collectorsversion.NewCollector(program)
}
