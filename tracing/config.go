package tracing

import (
	"flag"
)

type Config struct {
	OtelEndpoint string `yaml:"otel_endpoint"`
	OrgID        string `yaml:"org_id"`
	/* InstallOTBridge bool   `yaml:"install_opentracing_bridge"` */
}

func (c *Config) RegisterFlagsAndApplyDefaults(prefix string, f *flag.FlagSet) {
	f.StringVar(&c.OtelEndpoint, PrefixConfig(prefix, "otel.endpoint"), "", "otel endpoint, eg: tempo:4317")
	f.StringVar(&c.OrgID, PrefixConfig(prefix, "org.id"), "", "org ID to use when sending traces")
	/* f.BoolVar(&c.InstallOTBridge, PrefixConfig(prefix, "install.opentracing.bridge"), false, "enable the OpenTracing bridge") */
}

func PrefixConfig(prefix string, option string) string {
	if len(prefix) > 0 {
		return prefix + "." + option
	}

	return option
}
