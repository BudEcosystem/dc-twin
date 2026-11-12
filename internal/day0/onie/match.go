package onie

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BudEcosystem/dc-twin/internal/nos"
)

// RejectFile is the file in Dir whose presence raises the onie-image-mismatch fault while the
// program runs (packs/generic/faults/day0.yaml, day0.onieRejectAll): Dir is the host directory
// the twin writes and the switch mounts read-only, so the twin can raise and clear the fault
// without restarting ONIE.
const RejectFile = "onie-reject-all"

// RejectReason is the reason an installer fails under the fault.
const RejectReason = "the installer failed its checks (fault)"

// nameRegexp compiles an image's name. The name is a Go regexp over the fetched URL's last path
// element, matched whole (anchored), so `onie-installer` does not also recognise
// `onie-installer-x86_64.bin`.
func nameRegexp(name string) (*regexp.Regexp, error) {
	if name == "" {
		return nil, nil
	}
	return regexp.Compile(`^(?:` + name + `)$`)
}

// match recognises a fetched file (D75), returning the image it installs or why it fails.
//
// An image with a sha256 is recognised by it first. Failing that, an image with a name is
// recognised by the file's name; if that image also names a sha256, the file is a damaged or
// different build of it and fails, as an installer with a bad checksum fails. A recognised
// image for a NOS other than the one the twin's registry boots for this switch fails too: the
// twin can only boot the switch's own NOS (factory.json nos).
func (e *Env) match(sum, name string, size int64) (nos.Image, string) {
	var found *nos.Image
	for i, im := range e.images {
		if im.SHA256 != "" && strings.EqualFold(im.SHA256, sum) {
			found = &e.images[i]
			break
		}
	}
	if found == nil {
		for i, im := range e.images {
			re, _ := nameRegexp(im.Name)
			if re == nil || !re.MatchString(name) {
				continue
			}
			if im.SHA256 != "" {
				return nos.Image{}, fmt.Sprintf("%s is named as %s %s's installer but its sha256 is %s, not %s", name, im.NOS, im.Version, sum, strings.ToLower(im.SHA256))
			}
			found = &e.images[i]
			break
		}
	}
	if found == nil {
		return nos.Image{}, fmt.Sprintf("%s (%d bytes, sha256 %s) is no installer this switch recognises", name, size, sum)
	}
	if e.rejectAll() {
		return nos.Image{}, RejectReason
	}
	if found.NOS != e.factory.NOS {
		return nos.Image{}, fmt.Sprintf("%s is an installer for %s %s, not for this switch's %s", name, found.NOS, found.Version, e.factory.NOS)
	}
	return *found, ""
}

func (e *Env) rejectAll() bool {
	if e.RejectAll {
		return true
	}
	if e.Dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(e.Dir, RejectFile))
	return err == nil
}
