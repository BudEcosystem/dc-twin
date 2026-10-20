"""kmsg-relay.py <kmsg.log> <relay.log> -- <ssh argv to the second guest…>

The harness's one stand-in on the kernel-log path, and only for its last hop. compat/nvsentinel
uses it too, for its GPU guest.

The twin's in-host agent (internal/inhost) printk's a fault's catalogue lines into the kernel of
the twin's own `os: twin` guest (the XE9680's Alpine guest, under KVM) through its kernel module
and carries every record that kernel then holds back out, as /dev/kmsg gave it, into
state/inhost/<host>/kmsg.log ("<boot id> <prio>,<seq>,<usec>,<flags>;<text>"). That guest is musl
and has no way in for a cgo, glibc agent, so the agent under test runs in a second guest (Debian),
whose GPUs are the same server's through Mokka. This relay follows kmsg.log and enters every NVRM
record the twin's guest kernel recorded into the Debian guest's kernel, one write per record, at
the record's level, exactly as the in-host agent's guest half does: into /proc/dctwin-kmsg, the
same kernel module's (internal/inhost/kmod/guest-load.sh loads it there), which printk's it, a
kernel record with facility kern; or, where that guest has no module, into /dev/kmsg, where the
kernel gives it facility user. The text is the record's own, byte for byte, rendered by the twin
and recorded by a kernel; the Debian kernel gives it its own sequence number and timestamp.

Each relayed record is logged to <relay.log> as one JSON object per line: when it was seen, the
twin guest's boot id and record, what was written, and where ("via": /proc/dctwin-kmsg or
/dev/kmsg).
"""
import json
import os
import subprocess
import sys
import time


def main():
    if "--" not in sys.argv or sys.argv.index("--") != 3:
        sys.exit(__doc__)
    src, out, ssh = sys.argv[1], sys.argv[2], sys.argv[4:]
    seen = set()  # (boot, seq)
    pos = 0
    remote = ("sudo sh -c 'IFS= read -r l; f=/dev/kmsg; [ -w /proc/dctwin-kmsg ] && f=/proc/dctwin-kmsg; "
              "printf \"%s\\n\" \"$l\" > $f && echo $f'")
    with open(out, "a", buffering=1) as log:
        while True:
            try:
                size = os.path.getsize(src)
            except OSError:
                time.sleep(0.2)
                continue
            if size < pos:
                pos = 0
            if size == pos:
                time.sleep(0.1)
                continue
            with open(src, "rb") as f:
                f.seek(pos)
                chunk = f.read()
            # only whole lines; a partial last line is read again next time
            end = chunk.rfind(b"\n")
            if end < 0:
                time.sleep(0.1)
                continue
            pos += end + 1
            for raw in chunk[: end + 1].decode("utf-8", "replace").splitlines():
                boot, _, rec = raw.partition(" ")
                head, sep, text = rec.partition(";")
                f3 = head.split(",")
                if not sep or len(f3) < 3 or not text.startswith("NVRM:"):
                    continue
                try:
                    prio, seq = int(f3[0]), int(f3[1])
                except ValueError:
                    continue
                if (boot, seq) in seen:
                    continue
                seen.add((boot, seq))
                line = "<%d>%s" % (prio & 7, text)
                t0 = time.time()
                r = subprocess.run(ssh + [remote], input=(line + "\n").encode(), capture_output=True, timeout=30)
                log.write(json.dumps({"seen": t0, "written": time.time(), "boot": boot, "record": rec, "wrote": line,
                                      "via": r.stdout.decode(errors="replace").strip(),
                                      "ok": r.returncode == 0, "err": r.stderr.decode(errors="replace").strip()}) + "\n")


if __name__ == "__main__":
    main()
