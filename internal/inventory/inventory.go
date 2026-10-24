// Package inventory describes the computer the bridge runs on: what it is
// (its system, processor, memory, disks and network), sent to ViaVeritas when
// any of it changes. Who uses it (Personal) is sent only while the
// inventory_personal feature runs.
package inventory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
	gnet "github.com/shirou/gopsutil/v4/net"
)

// Inventory is the computer as it is now.
type Inventory struct {
	CollectedAt time.Time   `json:"collected_at"`
	Host        Host        `json:"host"`
	CPU         CPU         `json:"cpu"`
	Memory      Memory      `json:"memory"`
	Disks       []Disk      `json:"disks"`
	Network     Network     `json:"network"`
	Personal    *Personal   `json:"personal,omitempty"`
	Bridge      BridgeFacts `json:"bridge"`
}

// Host is the computer and its system.
type Host struct {
	Hostname        string `json:"hostname"`
	OS              string `json:"os"`
	Platform        string `json:"platform,omitempty"`
	PlatformFamily  string `json:"platform_family,omitempty"`
	PlatformVersion string `json:"platform_version,omitempty"`
	KernelVersion   string `json:"kernel_version,omitempty"`
	Arch            string `json:"arch,omitempty"`
	// Virtualization is "docker guest", "kvm host"…, empty on hardware.
	Virtualization string    `json:"virtualization,omitempty"`
	BootTime       time.Time `json:"boot_time"`
	Timezone       string    `json:"timezone,omitempty"`
	Manufacturer   string    `json:"manufacturer,omitempty"`
	Model          string    `json:"model,omitempty"`
	Serial         string    `json:"serial,omitempty"`
}

// CPU is its processor.
type CPU struct {
	Model   string  `json:"model,omitempty"`
	Vendor  string  `json:"vendor,omitempty"`
	Cores   int     `json:"cores"`
	Threads int     `json:"threads"`
	MHz     float64 `json:"mhz,omitempty"`
}

// Memory is how much it has.
type Memory struct {
	Total     uint64 `json:"total"`
	SwapTotal uint64 `json:"swap_total"`
}

// Disk is one volume worth showing: a real file system, not one the system keeps for itself.
type Disk struct {
	Mount   string `json:"mount"`
	Device  string `json:"device"`
	FS      string `json:"fs"`
	Total   uint64 `json:"total"`
	Network bool   `json:"network,omitempty"`
}

// Interface is a network interface that is up and isn't the loopback.
type Interface struct {
	Name      string   `json:"name"`
	MAC       string   `json:"mac,omitempty"`
	MTU       int      `json:"mtu,omitempty"`
	Addresses []string `json:"addresses"`
}

// Network is how it reaches the network.
type Network struct {
	Interfaces []Interface `json:"interfaces"`
	LANIP      string      `json:"lan_ip,omitempty"`
	Gateway    string      `json:"gateway,omitempty"`
	DNS        []string    `json:"dns,omitempty"`
}

// Personal is who uses the computer.
type Personal struct {
	Users []User `json:"users"`
}

// User is someone signed in to it.
type User struct {
	Name     string    `json:"name"`
	Terminal string    `json:"terminal,omitempty"`
	Since    time.Time `json:"since,omitempty"`
}

// BridgeFacts is how the bridge itself runs there.
type BridgeFacts struct {
	Version    string `json:"version"`
	InstallDir string `json:"install_dir"`
	PID        int    `json:"pid"`
}

// Collect describes the computer; personal adds who is signed in. Parts that
// can't be read are left out rather than failing the rest.
func Collect(ctx context.Context, personal bool, bridge BridgeFacts) Inventory {
	inv := Inventory{CollectedAt: time.Now().UTC(), Bridge: bridge, Disks: []Disk{}}
	inv.Host.OS = runtime.GOOS
	if info, err := host.InfoWithContext(ctx); err == nil {
		inv.Host.Hostname = info.Hostname
		inv.Host.Platform = info.Platform
		inv.Host.PlatformFamily = info.PlatformFamily
		inv.Host.PlatformVersion = info.PlatformVersion
		inv.Host.KernelVersion = info.KernelVersion
		inv.Host.Arch = info.KernelArch
		if info.VirtualizationSystem != "" {
			inv.Host.Virtualization = strings.TrimSpace(info.VirtualizationSystem + " " + info.VirtualizationRole)
		}
		if info.BootTime > 0 {
			inv.Host.BootTime = time.Unix(int64(info.BootTime), 0).UTC()
		}
	} else if name, err := os.Hostname(); err == nil {
		inv.Host.Hostname = name
	}
	inv.Host.Timezone = timezone()
	inv.Host.Manufacturer, inv.Host.Model, inv.Host.Serial = machine(ctx)

	if infos, err := cpu.InfoWithContext(ctx); err == nil && len(infos) > 0 {
		inv.CPU.Model = strings.TrimSpace(infos[0].ModelName)
		inv.CPU.Vendor = infos[0].VendorID
		inv.CPU.MHz = infos[0].Mhz
	}
	if n, err := cpu.CountsWithContext(ctx, false); err == nil {
		inv.CPU.Cores = n
	}
	if n, err := cpu.CountsWithContext(ctx, true); err == nil {
		inv.CPU.Threads = n
	}

	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		inv.Memory.Total = vm.Total
	}
	if sw, err := mem.SwapMemoryWithContext(ctx); err == nil {
		inv.Memory.SwapTotal = sw.Total
	}

	for _, v := range Volumes(ctx) {
		if u, err := disk.UsageWithContext(ctx, v.Mount); err == nil && u.Total > 0 {
			v.Total = u.Total
			inv.Disks = append(inv.Disks, v)
		}
	}

	inv.Network = network(ctx)

	if personal {
		inv.Personal = &Personal{Users: []User{}}
		seen := map[string]bool{}
		for _, u := range signedIn(ctx) {
			key := u.Name + "\x00" + u.Terminal
			if u.Name == "" || seen[key] {
				continue
			}
			seen[key] = true
			inv.Personal.Users = append(inv.Personal.Users, u)
		}
	}
	return inv
}

// Hash is what tells one inventory from another: everything but when it was
// collected and the bridge's own process.
func (inv Inventory) Hash() string {
	c := inv
	c.CollectedAt = time.Time{}
	c.Bridge.PID = 0
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// File systems that hold someone's files; the rest (proc, tmpfs, overlays,
// snaps…) are the system's own.
var realFS = map[string]bool{
	"apfs": true, "hfs": true, "ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "zfs": true,
	"f2fs": true, "jfs": true, "reiserfs": true, "vfat": true, "fat32": true, "exfat": true, "ntfs": true,
	"ntfs3": true, "fuseblk": true, "refs": true, "msdos": true,
}

var networkFS = map[string]bool{"nfs": true, "nfs4": true, "cifs": true, "smbfs": true, "smb3": true, "afpfs": true, "webdav": true}

// Volumes are the disks worth showing, one per device, without their size.
func Volumes(ctx context.Context) []Disk {
	parts, err := disk.PartitionsWithContext(ctx, false)
	if err != nil {
		return nil
	}
	var out []Disk
	seen := map[string]bool{}
	for _, p := range parts {
		fs := strings.ToLower(p.Fstype)
		network := networkFS[fs]
		if !realFS[fs] && !network {
			continue
		}
		if skipMount(p.Mountpoint) || (p.Device != "" && seen[p.Device]) {
			continue
		}
		seen[p.Device] = true
		out = append(out, Disk{Mount: p.Mountpoint, Device: p.Device, FS: p.Fstype, Network: network})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mount < out[j].Mount })
	return out
}

// skipMount leaves out what a system mounts for itself: macOS's system
// volumes, Linux's snaps and boot partitions.
func skipMount(m string) bool {
	switch runtime.GOOS {
	case "darwin":
		// The startup disk and what is plugged in; the rest are the system's volumes and mounted disk images.
		return m != "/" && !strings.HasPrefix(m, "/Volumes/")
	case "linux":
		return strings.HasPrefix(m, "/snap/") || strings.HasPrefix(m, "/boot") || strings.HasPrefix(m, "/var/lib/docker") || strings.HasPrefix(m, "/run/")
	}
	return false
}

// network is its interfaces that are up (not the loopback), its address on
// the local network, the gateway and the DNS servers.
func network(ctx context.Context) Network {
	n := Network{Interfaces: []Interface{}}
	ifaces, err := gnet.InterfacesWithContext(ctx)
	if err == nil {
		for _, it := range ifaces {
			if !slices.Contains(it.Flags, "up") || slices.Contains(it.Flags, "loopback") {
				continue
			}
			addrs := make([]string, 0, len(it.Addrs))
			reachable := false
			for _, a := range it.Addrs {
				addrs = append(addrs, a.Addr)
				reachable = reachable || !linkLocal(a.Addr)
			}
			// Interfaces with link-local addresses alone (AirDrop's, tunnels not up) reach nothing.
			if !reachable {
				continue
			}
			n.Interfaces = append(n.Interfaces, Interface{Name: it.Name, MAC: it.HardwareAddr, MTU: it.MTU, Addresses: addrs})
		}
	}
	n.LANIP = lanIP()
	n.Gateway = gateway(ctx)
	n.DNS = dnsServers(ctx)
	return n
}

// linkLocal reports whether an address (with its prefix) is only for its own link.
func linkLocal(addr string) bool {
	ip, _, err := net.ParseCIDR(addr)
	if err != nil {
		ip = net.ParseIP(addr)
	}
	return ip == nil || ip.IsLinkLocalUnicast() || ip.IsLoopback()
}

// lanIP is the address the computer reaches the internet from: the source of
// a UDP "connection", which sends nothing.
func lanIP() string {
	c, err := net.Dial("udp", "1.1.1.1:53")
	if err != nil {
		return ""
	}
	defer c.Close()
	if a, ok := c.LocalAddr().(*net.UDPAddr); ok {
		return a.IP.String()
	}
	return ""
}

func timezone() string {
	if tz := os.Getenv("TZ"); tz != "" {
		return tz
	}
	name, _ := time.Now().Zone()
	if loc := time.Local.String(); loc != "" && loc != "Local" {
		return loc
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(target, "zoneinfo/"); i >= 0 {
			return target[i+len("zoneinfo/"):]
		}
	}
	return name
}
