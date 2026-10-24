package inventory

import (
	"context"
	"strings"
)

// machine is the Mac's maker, model and serial number, as System Information says.
func machine(ctx context.Context) (manufacturer, model, serial string) {
	hw := run(ctx, "system_profiler", "SPHardwareDataType")
	model = field(hw, "Model Name")
	if id := field(hw, "Model Identifier"); id != "" {
		model = strings.TrimSpace(model + " (" + id + ")")
	}
	return "Apple", model, field(hw, "Serial Number (system)")
}

func gateway(ctx context.Context) string {
	return field(run(ctx, "route", "-n", "get", "default"), "gateway")
}

func dnsServers(ctx context.Context) []string {
	var out []string
	for _, line := range strings.Split(run(ctx, "scutil", "--dns"), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && strings.HasPrefix(strings.TrimSpace(k), "nameserver[") {
			out = append(out, v)
		}
	}
	return unique(out)
}
