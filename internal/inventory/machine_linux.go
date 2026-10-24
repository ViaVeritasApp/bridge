package inventory

import (
	"context"
	"encoding/hex"
	"net"
	"os"
	"strings"
)

func dmi(name string) string {
	b, err := os.ReadFile("/sys/class/dmi/id/" + name)
	if err != nil {
		return ""
	}
	v := strings.TrimSpace(string(b))
	// Firmware that leaves them blank fills them with this.
	if strings.EqualFold(v, "To be filled by O.E.M.") || strings.EqualFold(v, "Default string") || strings.EqualFold(v, "System Product Name") {
		return ""
	}
	return v
}

// machine is the computer's maker, model and serial number from its firmware
// (the serial needs root, which the service has).
func machine(context.Context) (manufacturer, model, serial string) {
	model = dmi("product_name")
	if v := dmi("product_version"); v != "" && !strings.EqualFold(v, "None") {
		model = strings.TrimSpace(model + " " + v)
	}
	if model == "" {
		// A Raspberry Pi and other boards name themselves in the device tree.
		if b, err := os.ReadFile("/proc/device-tree/model"); err == nil {
			model = strings.TrimRight(string(b), "\x00\n")
		}
	}
	return dmi("sys_vendor"), model, dmi("product_serial")
}

// gateway is the default route's, from the kernel's routing table.
func gateway(context.Context) string {
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 3 || f[1] != "00000000" {
			continue
		}
		raw, err := hex.DecodeString(f[2])
		if err != nil || len(raw) != 4 {
			continue
		}
		return net.IPv4(raw[3], raw[2], raw[1], raw[0]).String()
	}
	return ""
}

func dnsServers(ctx context.Context) []string {
	var out []string
	// systemd-resolved points resolv.conf at itself; the servers it uses are its own.
	if text := run(ctx, "resolvectl", "dns"); text != "" {
		for _, line := range strings.Split(text, "\n") {
			if _, v, ok := strings.Cut(line, ":"); ok {
				out = append(out, strings.Fields(v)...)
			}
		}
	}
	if b, err := os.ReadFile("/etc/resolv.conf"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) >= 2 && f[0] == "nameserver" && f[1] != "127.0.0.53" {
				out = append(out, f[1])
			}
		}
	}
	return unique(out)
}
