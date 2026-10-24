package inventory

import (
	"context"
	"strings"
)

func powershell(ctx context.Context, script string) string {
	return run(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
}

// machine is the computer's maker, model and serial number, as Windows reads them from its firmware.
func machine(ctx context.Context) (manufacturer, model, serial string) {
	out := powershell(ctx, "$c=Get-CimInstance Win32_ComputerSystem; $b=Get-CimInstance Win32_BIOS; \"$($c.Manufacturer)`n$($c.Model)`n$($b.SerialNumber)\"")
	lines := strings.Split(strings.ReplaceAll(out, "\r", ""), "\n")
	for len(lines) < 3 {
		lines = append(lines, "")
	}
	return strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1]), strings.TrimSpace(lines[2])
}

func gateway(ctx context.Context) string {
	return powershell(ctx, "(Get-NetRoute -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue | Sort-Object RouteMetric | Select-Object -First 1).NextHop")
}

func dnsServers(ctx context.Context) []string {
	out := powershell(ctx, "(Get-DnsClientServerAddress -AddressFamily IPv4 -ErrorAction SilentlyContinue).ServerAddresses")
	return unique(strings.Split(strings.ReplaceAll(out, "\r", ""), "\n"))
}
