// Package identity implements identity/v1 (docs/spec/13-schemas.md — Identity derivation):
//
//	prk      = HKDF-Extract(SHA-256, salt = "dc-twin/identity/v1", ikm = utf8(seed))
//	material = HKDF-Expand(SHA-256, prk, info = kind ‖ 0x00 ‖ path ‖ 0x00 ‖ profile@major ‖ 0x00 ‖ index, len)
//
// The derivation version is part of the apiVersion contract: nothing here may change within
// v1alpha1, because identities must be stable across up, apply and down/up.
package identity

import (
	"crypto/hkdf"
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"
)

const Salt = "dc-twin/identity/v1"

// Deriver derives identities for one estate seed.
type Deriver struct {
	prk []byte
}

func New(seed int64) *Deriver {
	prk, err := hkdf.Extract(sha256.New, []byte(strconv.FormatInt(seed, 10)), []byte(Salt))
	if err != nil {
		panic(err)
	}
	return &Deriver{prk: prk}
}

// Material returns n bytes for (kind, path, profile@major, index).
func (d *Deriver) Material(kind, path, profileMajor string, index int, n int) []byte {
	info := kind + "\x00" + path + "\x00" + profileMajor + "\x00" + strconv.Itoa(index)
	out, err := hkdf.Expand(sha256.New, d.prk, info, n)
	if err != nil {
		panic(err)
	}
	return out
}

// ProfileMajor turns "nvidia/gb300-compute-tray" + "0.1.0" into "nvidia/gb300-compute-tray@0".
func ProfileMajor(name, version string) string {
	major, _, _ := strings.Cut(version, ".")
	if major == "" {
		major = "0"
	}
	return name + "@" + major
}

const (
	digits = "0123456789"
	upper  = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	hexU   = "0123456789ABCDEF"
	alnum  = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	alnumL = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
)

// Serial renders a serial number through the profile's serialFormat: literal text plus
// {digits:n}, {upper:n}, {hex:n} and {alnum:n}.
func (d *Deriver) Serial(path, profileMajor, format string, index int) string {
	if format == "" {
		format = "DT{alnum:10}"
	}
	m := d.Material("serial", path, profileMajor, index, 64)
	var b strings.Builder
	k := 0
	next := func(alphabet string) byte {
		c := alphabet[int(m[k%len(m)])%len(alphabet)]
		k++
		return c
	}
	for i := 0; i < len(format); {
		if format[i] == '{' {
			end := strings.IndexByte(format[i:], '}')
			if end > 0 {
				spec := format[i+1 : i+end]
				name, ns, _ := strings.Cut(spec, ":")
				n, _ := strconv.Atoi(ns)
				alphabet := map[string]string{"digits": digits, "upper": upper, "hex": hexU, "alnum": alnum}[name]
				if alphabet != "" && n > 0 {
					for j := 0; j < n; j++ {
						b.WriteByte(next(alphabet))
					}
					i += end + 1
					continue
				}
			}
		}
		b.WriteByte(format[i])
		i++
	}
	return b.String()
}

// UUID is a version-4-shaped UUID from 16 bytes of material.
func (d *Deriver) UUID(path, profileMajor string, index int) string {
	m := d.Material("uuid", path, profileMajor, index, 16)
	m[6] = (m[6] & 0x0f) | 0x40
	m[8] = (m[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", m[0:4], m[4:6], m[6:8], m[8:10], m[10:16])
}

// MAC is the profile's OUI plus three derived bytes, for one port.
func (d *Deriver) MAC(path, profileMajor, port, oui string, index int) string {
	if oui == "" {
		oui = "02:dc:7e" // locally administered: profiles without an OUI never collide with a vendor
	}
	m := d.Material("mac:"+port, path, profileMajor, index, 3)
	return strings.ToLower(fmt.Sprintf("%s:%02x:%02x:%02x", oui, m[0], m[1], m[2]))
}

// GUID is the profile's prefix plus five derived bytes (InfiniBand port GUIDs).
func (d *Deriver) GUID(path, profileMajor, port, prefix string, index int) string {
	if prefix == "" {
		prefix = "0xb83fd2"
	}
	// An InfiniBand GUID is 64 bits: the profile's prefix and as many derived bytes as fill them
	// (5 after the default three-byte OUI prefix). HKDF output is prefix-stable, so a shorter tail
	// is the leading bytes of the longer one.
	n := 8 - len(strings.TrimPrefix(strings.ToLower(prefix), "0x"))/2
	if n < 1 {
		n = 1
	}
	m := d.Material("guid:"+port, path, profileMajor, index, n)
	return fmt.Sprintf("%s%x", strings.ToLower(prefix), m)
}

// Password is the factory BMC password: 16 alphanumerics.
func (d *Deriver) Password(path, profileMajor string) string {
	m := d.Material("bmc-password", path, profileMajor, 0, 16)
	var b strings.Builder
	for _, c := range m {
		b.WriteByte(alnumL[int(c)%len(alnumL)])
	}
	return b.String()
}

// Scrub maps an identity value found in a captured mockup to a derived one, so equal values in
// the capture stay equal on the twin and no captured device's identity is served.
func (d *Deriver) Scrub(kind, path, profileMajor, original string) string {
	m := d.Material("scrub:"+kind+":"+original, path, profileMajor, 0, 32)
	switch kind {
	case "mac":
		sep := ":"
		if strings.Contains(original, "-") {
			sep = "-"
		}
		upperCase := original != strings.ToLower(original)
		parts := strings.Split(strings.ReplaceAll(original, "-", ":"), ":")
		if len(parts) != 6 {
			return original
		}
		s := fmt.Sprintf("%s%s%s%s%s%s%02x%s%02x%s%02x", parts[0], sep, parts[1], sep, parts[2], sep, m[0], sep, m[1], sep, m[2])
		if upperCase {
			return strings.ToUpper(s)
		}
		return strings.ToLower(s)
	case "uuid":
		if len(original) != 36 {
			return original
		}
		m[6] = (m[6] & 0x0f) | 0x40
		m[8] = (m[8] & 0x3f) | 0x80
		s := fmt.Sprintf("%x-%x-%x-%x-%x", m[0:4], m[4:6], m[6:8], m[8:10], m[10:16])
		if original != strings.ToLower(original) {
			return strings.ToUpper(s)
		}
		return s
	default: // serial and anything shaped like one: keep the shape, replace the characters
		var b strings.Builder
		k := 0
		for _, r := range original {
			switch {
			case r >= '0' && r <= '9':
				b.WriteByte(digits[int(m[k%32])%10])
				k++
			case r >= 'A' && r <= 'Z':
				b.WriteByte(upper[int(m[k%32])%26])
				k++
			case r >= 'a' && r <= 'z':
				b.WriteByte(upper[int(m[k%32])%26] + 32)
				k++
			default:
				b.WriteRune(r)
			}
		}
		return b.String()
	}
}
