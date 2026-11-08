package metrics

import (
	"fmt"
	"io"
	"strings"

	dto "github.com/prometheus/client_model/go"

	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
)

// The storage exporters (docs/reviews/2026-11-08-storage-deep-dive.md 5.1): node_exporter's NVMe,
// disk and file-system families and smartctl_exporter's device families, served for every model
// host with drives, from the model's drives (engine.DriveStatus). Names and labels are the
// exporters' at the releases named here (node_exporter collector/nvme_linux.go,
// diskstats_linux.go, filesystem_common.go; smartctl_exporter metrics.go).

// StorageExporters are served beside a host's accelerator exporter, on every model host that has
// drives.
var StorageExporters = []*Exporter{
	{ID: "node", Impersonator: "node_exporter", Upstream: "prometheus/node_exporter", Version: "v1.12.1", Port: 9100, render: renderNode},
	{ID: "smartctl", Impersonator: "smartctl_exporter", Upstream: "prometheus-community/smartctl_exporter", Version: "v0.15.0", Port: 9633, render: renderSmartctl},
}

// driveSnap is a drive as a scrape sees it: the device name the host's kernel gives it, and its
// status.
type driveSnap struct {
	c      *model.Component
	dev    string // nvme<n> for an NVMe drive, sd<x> for one behind a controller or SATA/SAS
	ns     string // the block device: nvme<n>n1, or sd<x>
	st     engine.DriveStatus
	volume bool // a RAID volume the host sees in place of its drives
	size   int64
	model  string
}

// storageSnap reads the host's drives: NVMe drives on PCIe as nvme0.., the rest (and a RAID
// controller's volumes, which the host sees instead of their members) as sda...
func storageSnap(e *engine.Engine, d *model.Device, uptimeS float64) (drives []driveSnap, availPct float64) {
	nvme, sd := 0, 0
	sdName := func() string {
		n := sd
		sd++
		s := ""
		for {
			s = string(rune('a'+n%26)) + s
			n = n/26 - 1
			if n < 0 {
				return "sd" + s
			}
		}
	}
	var behind []*model.Component
	for _, c := range d.Drives() {
		st := e.DriveStatus(c)
		if c.Drive.Controller != nil {
			behind = append(behind, c)
			continue
		}
		if !st.Present || st.State == "UnavailableOffline" {
			continue // gone from the host's view
		}
		ds := driveSnap{c: c, st: st, size: c.Drive.CapacityBytes, model: c.Model}
		if c.Drive.Protocol == "nvme" {
			ds.dev = fmt.Sprintf("nvme%d", nvme)
			ds.ns = ds.dev + "n1"
			nvme++
		} else {
			ds.dev = sdName()
			ds.ns = ds.dev
		}
		drives = append(drives, ds)
	}
	for _, v := range e.Volumes(d) {
		if v.Pending == "create" || v.State == "UnavailableOffline" {
			continue
		}
		name := sdName()
		drives = append(drives, driveSnap{dev: name, ns: name, volume: true, size: v.CapacityBytes, model: v.RAIDType + " Virtual Disk",
			st: engine.DriveStatus{Present: true, Health: v.Health, State: v.State}})
	}
	_ = behind
	availPct = 70
	if v, ok := e.FaultSets(d)["filesystem.availPercent"]; ok {
		availPct = toF(v)
	}
	return drives, availPct
}

func toF(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	}
	var f float64
	fmt.Sscan(fmt.Sprint(v), &f)
	return f
}

// renderNode serves node_exporter's nvme, diskstats and filesystem collectors.
func renderNode(w io.Writer, s *snapshot) error {
	g := func(name, help string) *family { return &family{name: name, help: help, typ: dto.MetricType_GAUGE} }
	c := func(name, help string) *family { return &family{name: name, help: help, typ: dto.MetricType_COUNTER} }
	nvmeInfo := g("node_nvme_info", "Non-numeric data from /sys/class/nvme/<device>, value is always 1.")
	nsCap := g("node_nvme_namespace_capacity_bytes", "Capacity of the NVMe namespace in bytes. Computed as namespace_size * namespace_logical_block_size")
	nsSize := g("node_nvme_namespace_size_bytes", "Size of the NVMe namespace in bytes. Available in /sys/class/nvme/<device>/<namespace>/size")
	nsUsed := g("node_nvme_namespace_used_bytes", "Used space of the NVMe namespace in bytes. Available in /sys/class/nvme/<device>/<namespace>/nuse")
	nsLBS := g("node_nvme_namespace_logical_block_size_bytes", "Logical block size of the NVMe namespace in bytes. Usually 4Kb. Available in /sys/class/nvme/<device>/<namespace>/queue/logical_block_size")
	diskInfo := g("node_disk_info", "Info of /sys/block/<block_device>.")
	readBytes := c("node_disk_read_bytes_total", "The total number of bytes read successfully.")
	writeBytes := c("node_disk_written_bytes_total", "The total number of bytes written successfully.")
	reads := c("node_disk_reads_completed_total", "The total number of reads completed successfully.")
	writes := c("node_disk_writes_completed_total", "The total number of writes completed successfully.")
	ioTime := c("node_disk_io_time_seconds_total", "Total seconds spent doing I/Os.")
	fsSize := g("node_filesystem_size_bytes", "Filesystem size in bytes.")
	fsFree := g("node_filesystem_free_bytes", "Filesystem free space in bytes.")
	fsAvail := g("node_filesystem_avail_bytes", "Filesystem space available to non-root users in bytes.")
	fsRO := g("node_filesystem_readonly", "Filesystem read-only status.")
	minor := 0
	for _, d := range s.drives {
		rot := "0"
		if d.c != nil && d.c.Drive.Media == "hdd" {
			rot = "1"
		}
		serial, fw, wwn := "", "", ""
		if d.c != nil {
			serial, fw, wwn = d.st.Serial, d.c.Drive.Firmware, "eui."+strings.ToLower(d.st.EUI64)
		}
		if strings.HasPrefix(d.dev, "nvme") {
			nvmeInfo.add(1, label{"cntlid", "0"}, label{"device", d.dev}, label{"firmware_revision", fw}, label{"model", d.model}, label{"serial", serial}, label{"state", "live"})
			nsCap.add(float64(d.size), label{"device", d.ns}, label{"nsid", "1"})
			nsSize.add(float64(d.size), label{"device", d.ns}, label{"nsid", "1"})
			nsUsed.add(float64(d.size)*0.3, label{"device", d.ns}, label{"nsid", "1"})
			nsLBS.add(512, label{"device", d.ns}, label{"nsid", "1"})
		}
		major := "259"
		if !strings.HasPrefix(d.dev, "nvme") {
			major, wwn = "8", ""
		}
		diskInfo.add(1, label{"device", d.ns}, label{"major", major}, label{"minor", fmt.Sprint(minor)}, label{"model", d.model},
			label{"path", ""}, label{"revision", fw}, label{"rotational", rot}, label{"serial", serial}, label{"wwn", wwn})
		minor++
		// I/O at the host's utilisation since it booted: a rate the model implies, not a measurement
		rate := 200e6 * s.util
		readBytes.add(round2(rate*s.uptimeS), label{"device", d.ns})
		writeBytes.add(round2(rate*0.4*s.uptimeS), label{"device", d.ns})
		reads.add(round2(rate*s.uptimeS/131072), label{"device", d.ns})
		writes.add(round2(rate*0.4*s.uptimeS/131072), label{"device", d.ns})
		ioTime.add(round2(s.util*s.uptimeS), label{"device", d.ns})
	}
	// the root file system is on the boot drive (its second partition, after the EFI one)
	if bd := s.ep.Device.BootDrive(); bd != nil {
		for _, d := range s.drives {
			if d.c == bd {
				dev := "/dev/" + d.ns
				if strings.HasPrefix(d.ns, "nvme") {
					dev += "p2"
				} else {
					dev += "2"
				}
				size := float64(d.size) * 0.98
				ls := []label{{"device", dev}, {"device_error", ""}, {"fstype", "ext4"}, {"mountpoint", "/"}}
				fsSize.add(round2(size), ls...)
				fsFree.add(round2(size*s.availPct/100+size*0.05), ls...)
				fsAvail.add(round2(size*s.availPct/100), ls...)
				ro := 0.0
				if d.st.ReadOnly {
					ro = 1
				}
				fsRO.add(ro, ls...)
			}
		}
	}
	return write(w, []*family{nvmeInfo, nsCap, nsSize, nsUsed, nsLBS, diskInfo, readBytes, writeBytes, reads, writes, ioTime, fsSize, fsFree, fsAvail, fsRO}, true)
}

// renderSmartctl serves smartctl_exporter's device families.
func renderSmartctl(w io.Writer, s *snapshot) error {
	g := func(name, help string) *family { return &family{name: name, help: help, typ: dto.MetricType_GAUGE} }
	count := g("smartctl_devices", "Number of devices configured or dynamically discovered")
	info := g("smartctl_device", "Device info")
	capB := g("smartctl_device_capacity_bytes", "Device capacity in bytes")
	nvmeCap := g("smartctl_device_nvme_capacity_bytes", "NVMe device total capacity bytes")
	poh := g("smartctl_device_power_on_seconds", "Device power on seconds")
	temp := g("smartctl_device_temperature", "Device temperature celsius")
	used := g("smartctl_device_percentage_used", "Device write percentage used")
	spare := g("smartctl_device_available_spare", "Normalized percentage (0 to 100%) of the remaining spare capacity available")
	spareT := g("smartctl_device_available_spare_threshold", "When the Available Spare falls below the threshold indicated in this field, an asynchronous event completion may occur. The value is indicated as a normalized percentage (0 to 100%)")
	crit := g("smartctl_device_critical_warning", "This field indicates critical warnings for the state of the controller")
	media := g("smartctl_device_media_errors", "Contains the number of occurrences where the controller detected an unrecovered data integrity error. Errors such as uncorrectable ECC, CRC checksum failure, or LBA tag mismatch are included in this field")
	status := g("smartctl_device_smart_status", "General smart status")
	exit := g("smartctl_device_smartctl_exit_status", "Exit status of smartctl on device")
	rot := g("smartctl_device_rotation_rate", "Device rotation rate")
	n := 0
	for _, d := range s.drives {
		if d.volume || d.c == nil {
			continue
		}
		n++
		di := d.c.Drive
		iface, proto := "nvme", "NVMe"
		switch di.Protocol {
		case "sata":
			iface, proto = "sat", "ATA"
		case "sas":
			iface, proto = "scsi", "SCSI"
		}
		dev := d.dev
		info.add(1, label{"device", dev}, label{"interface", iface}, label{"protocol", proto}, label{"model_family", ""}, label{"model_name", d.model},
			label{"serial_number", d.st.Serial}, label{"ata_additional_product_id", ""}, label{"firmware_version", di.Firmware}, label{"ata_version", ""},
			label{"sata_version", ""}, label{"form_factor", di.FormFactor}, label{"scsi_vendor", ""}, label{"scsi_product", ""}, label{"scsi_revision", ""}, label{"scsi_version", ""})
		capB.add(float64(di.CapacityBytes), label{"device", dev})
		if di.Protocol == "nvme" {
			nvmeCap.add(float64(di.CapacityBytes), label{"device", dev})
			used.add(d.st.PercentageUsed, label{"device", dev})
			spare.add(d.st.AvailableSpare, label{"device", dev})
			spareT.add(10, label{"device", dev})
			crit.add(float64(d.st.CriticalWarning), label{"device", dev})
			media.add(d.st.MediaErrors, label{"device", dev})
		}
		if di.Media == "hdd" {
			rot.add(7200, label{"device", dev})
		}
		poh.add(round2(d.st.PowerOnHours*3600), label{"device", dev})
		temp.add(round2(d.st.TemperatureC), label{"device", dev}, label{"temperature_type", "current"})
		ok, code := 1.0, 0.0
		if d.st.CriticalWarning != 0 || d.st.FailurePredicted || d.st.Health == model.Critical {
			ok, code = 0, 8 // smartctl: "SMART status check returned DISK FAILING"
		}
		status.add(ok, label{"device", dev})
		exit.add(code, label{"device", dev})
	}
	count.add(float64(n))
	return write(w, []*family{count, info, capB, nvmeCap, poh, temp, used, spare, spareT, crit, media, status, exit, rot}, true)
}
