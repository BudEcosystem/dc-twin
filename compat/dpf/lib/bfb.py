"""List a BlueField boot stream (BFB) and extract images from it.

The header (24 bytes, little-endian) is the one NVIDIA's mlx-mkbfb (Mellanox/bfscripts) writes:
  word 0: magic 0x13026642 | major:4 @32 | minor:4 @36 | next_ver:4 @44 | cur_ver:4 @48 |
          header length / 8 :4 @52 | image id:8 @56
  word 1: image length (32 bits) | CRC-32 of the data and its padding (32 bits)
  word 2: bitmap of the image ids that follow (0 on the last)
The data follows, padded to a multiple of 8 bytes; every image's CRC is checked.

    python3 -I bfb.py <bfb> [<out dir> <id>[,<id>…]]

Image 62 is the Linux Image, 63 the installer initramfs, 50 the software inventory. The Image is
stored compressed (LZMA in the Ubuntu BFBs) and is written out uncompressed, as QEMU and UEFI load
it; its arm64 header's page size is printed.
"""
import binascii
import gzip
import lzma
import os
import struct
import sys

NAMES = {36: "psc-bl", 37: "psc-fw", 31: "bl2r-cert", 30: "bl2r", 42: "bl2r-auth-cert", 6: "bl2-cert", 1: "bl2",
         29: "sys", 43: "bl2-auth-cert", 38: "ddr-cert", 32: "ddr_ini", 33: "snps_images", 40: "ddr5_snps_images",
         34: "ddr_ate_imem", 35: "ddr_ate_dmem", 8: "bl30-key-cert", 12: "bl30-cert", 2: "bl30", 39: "psc-app",
         53: "psc-certs", 7: "trusted-key-cert", 9: "bl31-key-cert", 13: "bl31-cert", 3: "bl31", 10: "bl32-key-cert",
         14: "bl32-cert", 4: "bl32", 11: "bl33-key-cert", 15: "bl33-cert", 5: "bl33", 52: "capsule", 55: "boot-acpi",
         56: "boot-dtb", 57: "boot-desc", 58: "boot-path", 59: "boot-args", 60: "boot-timeout", 61: "uefi-tests",
         41: "nicfw", 54: "ramdisk", 49: "dpu-gi", 48: "cfg-gi", 50: "info", 62: "image", 63: "initramfs",
         51: "upgrade-image"}
MAGIC = 0x13026642
CHUNK = 8 << 20


def arm64_page_size(path):
    with open(path, "rb") as f:
        hdr = f.read(64)
    flags = struct.unpack_from("<Q", hdr, 24)[0]
    magic = hdr[56:60]
    if magic != b"ARMd":
        return "not an arm64 Image"
    return {0: "unspecified", 1: "4K", 2: "16K", 3: "64K"}[(flags >> 1) & 3]


def unpack(path):
    """Decompress an extracted Image in place when it is LZMA, xz or gzip compressed."""
    with open(path, "rb") as f:
        data = f.read()
    if data[:2] == b"\x1f\x8b":
        data = gzip.decompress(data)
    elif data[:6] == b"\xfd7zXZ\x00" or data[:3] == b"\x5d\x00\x00":
        data = lzma.decompress(data, format=lzma.FORMAT_AUTO)
    else:
        return
    with open(path, "wb") as f:
        f.write(data)


def main():
    path = sys.argv[1]
    outdir = sys.argv[2] if len(sys.argv) > 2 else None
    want = set(int(x) for x in sys.argv[3].split(",")) if len(sys.argv) > 3 else set()
    seen = {}
    with open(path, "rb") as f:
        n = 0
        while True:
            off = f.tell()
            h = f.read(24)
            if len(h) < 24:
                break
            w0, w1, follow = struct.unpack("<QQQ", h)
            if w0 & 0xFFFFFFFF != MAGIC:
                print("no BFB header at offset %d: %d bytes follow the stream" % (off, os.path.getsize(path) - off))
                break
            hl = ((w0 >> 52) & 0xF) * 8
            iid = (w0 >> 56) & 0xFF
            ilen, icrc = w1 & 0xFFFFFFFF, (w1 >> 32) & 0xFFFFFFFF
            if hl > 24:
                f.seek(off + hl)
            name = NAMES.get(iid, "id-%d" % iid)
            k = seen.get(iid, 0)
            seen[iid] = k + 1
            out = None
            if outdir and iid in want:
                fn = os.path.join(outdir, "%s%s.bin" % (name, "" if k == 0 else "-%d" % k))
                out = open(fn, "wb")
            crc, rem = 0, ilen
            while rem:
                b = f.read(min(CHUNK, rem))
                if not b:
                    raise SystemExit("truncated image %d (%s)" % (n, name))
                crc = binascii.crc32(b, crc)
                if out:
                    out.write(b)
                rem -= len(b)
            pad = ((ilen + 7) & ~7) - ilen
            crc = binascii.crc32(f.read(pad), crc) & 0xFFFFFFFF
            if out:
                out.close()
                if iid == 62:
                    unpack(fn)
                    print("   arm64 Image page size: %s" % arm64_page_size(fn))
            print("%2d off=%-11d id=%-3d %-18s cur_ver=%d len=%-11d crc %s" % (
                n, off, iid, name, (w0 >> 48) & 0xF, ilen, "ok" if crc == icrc else "MISMATCH"))
            n += 1
            if follow == 0:
                rest = os.path.getsize(path) - f.tell()
                if rest:
                    print("%d bytes after the last image (an appended bf.cfg)" % rest)
                break


main()
