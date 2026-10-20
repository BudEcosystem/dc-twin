package bluefield

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"regexp"
)

// A BlueField BMC takes its own firmware as a PLDM firmware update package (DMTF DSP0267): a
// header naming the package, the devices it applies to and its component images, then the
// images. NVIDIA's BF3 BMC package (bf3-bmc-<ver>_opn.fwpkg, the one DPF pushes when a BMC is
// older than it supports) names its package and components after the CEC ("cec1736ApFwPkg-
// 02410017"); the version the BMC reports once it runs the new image (BMC_Firmware's Version,
// "BF-24.10-17") is the one the BMC image carries, so that is what the twin reads.

// pldmHeaderUUIDs are the package header identifiers of DSP0267 1.0 and 1.1 (and later
// revisions that keep the 1.1 layout for what is read here).
var pldmHeaderUUIDs = [][]byte{
	{0xF0, 0x18, 0x87, 0x8C, 0xCB, 0x7D, 0x49, 0x43, 0x98, 0x00, 0xA0, 0x2F, 0x05, 0x9A, 0xCA, 0x02}, // 1.0
	{0x12, 0x44, 0xD2, 0x64, 0x8D, 0x7D, 0x47, 0x18, 0xA0, 0x30, 0xFC, 0x8A, 0x56, 0x58, 0x7D, 0x5A}, // 1.1
	{0x31, 0x19, 0xCE, 0x2F, 0xE8, 0x0A, 0x4A, 0x99, 0xAF, 0x6D, 0x46, 0xF8, 0xB1, 0x21, 0xF6, 0xBF}, // 1.2
	{0x7B, 0x29, 0x1C, 0x99, 0x6D, 0xB6, 0x42, 0x08, 0x80, 0x1B, 0x02, 0x02, 0x6E, 0x46, 0x3C, 0x78}, // 1.3
}

// pldmPackage is what the twin reads of a package.
type pldmPackage struct {
	version    string   // PackageVersionString
	components []string // ComponentVersionString of each component image
	bmcVersion string   // the BlueField BMC version its image carries ("BF-24.10-17"), if any
}

var bfBMCVersion = regexp.MustCompile(`BF-\d{2}\.\d{2}-\d+`)

// parsePLDM reads a DSP0267 package header and the version its BMC image carries.
func parsePLDM(b []byte) (*pldmPackage, error) {
	if len(b) < 36 {
		return nil, fmt.Errorf("too short for a PLDM firmware package")
	}
	known := false
	for _, u := range pldmHeaderUUIDs {
		if bytes.Equal(b[:16], u) {
			known = true
		}
	}
	if !known {
		return nil, fmt.Errorf("not a PLDM firmware update package (package header identifier %x)", b[:16])
	}
	o := 16
	o++ // PackageHeaderFormatRevision
	hsize := int(binary.LittleEndian.Uint16(b[o:]))
	o += 2
	o += 13 // PackageReleaseDateTime
	bitmapBits := int(binary.LittleEndian.Uint16(b[o:]))
	o += 2
	o++ // PackageVersionStringType
	vl := int(b[o])
	o++
	if o+vl > len(b) || hsize > len(b) {
		return nil, fmt.Errorf("PLDM package header overruns the package")
	}
	p := &pldmPackage{version: string(b[o : o+vl])}
	o += vl
	// FirmwareDeviceIdentificationArea: records of RecordLength bytes
	if o >= len(b) {
		return nil, fmt.Errorf("PLDM package header truncated")
	}
	records := int(b[o])
	o++
	for i := 0; i < records; i++ {
		if o+2 > len(b) {
			return nil, fmt.Errorf("PLDM device record %d truncated", i)
		}
		rl := int(binary.LittleEndian.Uint16(b[o:]))
		if rl < 11 || o+rl > len(b) {
			return nil, fmt.Errorf("PLDM device record %d has length %d", i, rl)
		}
		o += rl
	}
	// ComponentImageInformationArea
	if o+2 > len(b) {
		return nil, fmt.Errorf("PLDM component area truncated")
	}
	count := int(binary.LittleEndian.Uint16(b[o:]))
	o += 2
	var images [][2]int
	for i := 0; i < count; i++ {
		if o+22 > len(b) {
			return nil, fmt.Errorf("PLDM component %d truncated", i)
		}
		loc := int(binary.LittleEndian.Uint32(b[o+12:]))
		size := int(binary.LittleEndian.Uint32(b[o+16:]))
		cvl := int(b[o+21])
		o += 22
		if o+cvl > len(b) {
			return nil, fmt.Errorf("PLDM component %d version string truncated", i)
		}
		p.components = append(p.components, string(b[o:o+cvl]))
		o += cvl
		images = append(images, [2]int{loc, size})
	}
	_ = bitmapBits
	for _, im := range images {
		end := im[0] + im[1]
		if im[0] < 0 || end > len(b) || im[0] >= end {
			return nil, fmt.Errorf("PLDM component image at %d (%d bytes) lies outside the package (%d bytes)", im[0], im[1], len(b))
		}
		if v := bfBMCVersion.Find(b[im[0]:end]); v != nil && p.bmcVersion == "" {
			p.bmcVersion = string(v)
		}
	}
	return p, nil
}
