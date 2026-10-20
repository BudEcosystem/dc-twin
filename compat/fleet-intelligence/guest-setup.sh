#!/bin/sh
# guest-setup.sh <dir>: run as root inside the Debian guest run.sh boots. It makes the guest an
# NVIDIA GPU host as far as NVML is concerned, with GPUs that are the twin's, and installs NVIDIA's
# Fleet Intelligence agent on it from its release package:
#
#   1. Mokka (nvml-mock 0.5.0): its node agent, from the released image, runs as a systemd service
#      on the guest itself (--host-root=/), as the nvml-mock DaemonSet runs it on a node, with the
#      GPU profile dctwin-mokka rendered from the model. It stages the driver tree under
#      /var/lib/nvml-mock/driver (the mock libnvidia-ml.so.<driver version>, nvidia-smi, the
#      engine's config.yaml, the /dev/nvidia* nodes of that tree, /proc/driver/nvidia's files) and
#      bind-mounts it on /run/nvidia/driver. The mock finds its config.yaml, and the runtime
#      overrides.yaml beside it, from the path it was loaded from (/proc/self/maps), so every
#      process must load the tree's copy: the tree's lib64 is first on the dynamic linker's path
#      (ld.so.conf.d/00-…), ahead of /usr/local/lib, where the node agent reads the image's copy
#      from and where ldconfig links it too (a process that loaded that one would read
#      /etc/nvml-mock/config.yaml and an overrides.yaml nothing writes: GPUs that never fail).
#      The tree's libnvidia-ml.so.1 and libcuda.so.1 are also linked into /usr/lib/x86_64-linux-gnu,
#      where a driver install puts them and fleetint's library check looks. Setup fails unless a
#      process's dlopen("libnvidia-ml.so.1") maps the tree's file. Mokka's own kernel-log
#      announcement of an injected Xid (--kernel-log) is off: the Xid lines in this guest's kernel
#      log are the twin's.
#   2. fleetint 1.5.0 from its .deb. Its three Depends (datacenter-gpu-manager-4-proprietary,
#      nvattest, corelib) are NVIDIA's CUDA-repository packages for a host with a real driver, and
#      are not installed (dpkg --force-depends): the binary needs only libc and dlopens NVML and
#      DCGM. The package's own unit (fleetintd.service) runs it, configured through its own
#      /etc/default/fleetint: offline mode (files under /var/lib/fleetint-offline, every
#      FLEETINT_COLLECT_INTERVAL), its local API on its default unix socket, attestation and
#      inventory off (no backend is enrolled). One drop-in orders it after Mokka's service, as a
#      host's driver is up before its agents.
#
#   3. The in-host agent's kernel module (dctwin-kmsg, internal/inhost/kmod, D50), built here for
#      this guest's kernel (guest-load.sh installs linux-headers-<release> for it) and loaded on
#      every boot: kmsg-relay.py enters each NVRM record the twin's guest kernel recorded into this
#      kernel through it, so that the record is a printk, facility kern, as a driver's line is. The
#      result (loaded, or absent and why) is left in /var/lib/dctwin-kmod.txt; without the module
#      the relay writes through /dev/kmsg (facility user), and says so.
#
# <dir> holds what run.sh staged: mokka/bin/{node-agent,nvml-mock-ctl,nvidia-smi},
# mokka/lib/{libnvidia-ml,libcuda}.so.<ver>, config.yaml (the rendered profile), guest.env (NODE_NAME,
# GPU_COUNT, DRIVER_VERSION), kmod/{Kbuild,dctwin_kmsg.c,guest-load.sh} and fleetint_<ver>_amd64.deb.
set -eu
D=$1
. "$D/guest.env"

# ---------------------------------------------------------------- Mokka
install -m 0755 "$D/mokka/bin/node-agent" "$D/mokka/bin/nvml-mock-ctl" "$D/mokka/bin/nvidia-smi" /usr/local/bin/
# the node agent stages /usr/local/lib/{libnvidia-ml,libcuda}.so.*.*.* (its image's path) into the driver tree
install -m 0644 "$D"/mokka/lib/lib*.so.*.*.* /usr/local/lib/
install -D -m 0644 "$D/config.yaml" /etc/nvml-mock/config.yaml
cat >/etc/systemd/system/dctwin-nvml-mock.service <<EOF
# compat/fleet-intelligence: Mokka's node agent on this host, as the nvml-mock DaemonSet runs it
[Unit]
Description=Mokka nvml-mock node agent (GPUs rendered from the dc-twin model)
After=local-fs.target

[Service]
Environment=NODE_NAME=$NODE_NAME GPU_COUNT=$GPU_COUNT DRIVER_VERSION=$DRIVER_VERSION MOCK_NVML_KMSG=
ExecStart=/usr/local/bin/node-agent start --config=/etc/nvml-mock/config.yaml --host-root=/ --health-addr=127.0.0.1:9091 --log-format=plain --shutdown-timeout=5s --kernel-log=
# ready once the driver tree is staged and published: what a driver's service gives its dependants
ExecStartPost=/bin/sh -c 'for i in \$(seq 1 120); do [ -e /var/lib/nvml-mock/driver/usr/lib64/libnvidia-ml.so.1 ] && [ -e /run/nvidia/driver/usr/lib64/libnvidia-ml.so.1 ] && exit 0; sleep 0.5; done; exit 1'
Restart=on-failure

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now dctwin-nvml-mock.service
echo /var/lib/nvml-mock/driver/usr/lib64 >/etc/ld.so.conf.d/00-nvml-mock.conf
for l in libnvidia-ml.so.1 libcuda.so.1; do
	if [ -e "/var/lib/nvml-mock/driver/usr/lib64/$l" ]; then
		ln -sfn "/var/lib/nvml-mock/driver/usr/lib64/$l" "/usr/lib/x86_64-linux-gnu/$l"
	fi
done
ldconfig
nvml=$(python3 -I -c 'import ctypes; ctypes.CDLL("libnvidia-ml.so.1"); print(next((l.split()[-1] for l in open("/proc/self/maps") if "libnvidia-ml" in l), ""))')
echo "guest-setup: dlopen(libnvidia-ml.so.1) maps $nvml"
case "$nvml" in
/var/lib/nvml-mock/driver/*) ;;
*) echo "libnvidia-ml.so.1 resolves to $nvml, not the driver tree's copy: the mock would not see its overrides" >&2; exit 1 ;;
esac

# ---------------------------------------------------------------- the kernel module the relay writes through
sh "$D/kmod/guest-load.sh" "$D/kmod" | tee /var/lib/dctwin-kmod.txt | sed 's/^/guest-setup: kernel module dctwin-kmsg: /'

# ---------------------------------------------------------------- fleetint
# the package's postinst starts its service at once; policy-rc.d holds that until it is configured
printf '#!/bin/sh\nexit 101\n' >/usr/sbin/policy-rc.d
chmod 0755 /usr/sbin/policy-rc.d
dpkg --force-depends -i "$D"/fleetint_*_amd64.deb
rm -f /usr/sbin/policy-rc.d
f=/etc/default/fleetint
set_default() { # key value: replaces the key's line, or appends it
	if grep -q "^$1=" "$f"; then sed -i "s|^$1=.*|$1=\"$2\"|" "$f"; else printf '%s="%s"\n' "$1" "$2" >>"$f"; fi
}
set_default FLEETINT_FLAGS "--log-level=info --offline-mode --path=/var/lib/fleetint-offline --duration=06:00:00 --format=json"
set_default FLEETINT_COLLECT_INTERVAL 15s
set_default FLEETINT_CHECK_INTERVAL 15s
set_default FLEETINT_METRICS_LOOKBACK 1m
set_default FLEETINT_EVENTS_LOOKBACK 1h
set_default FLEETINT_ATTESTATION_ENABLED false
set_default FLEETINT_INVENTORY_ENABLED false
mkdir -p /etc/systemd/system/fleetintd.service.d
cat >/etc/systemd/system/fleetintd.service.d/10-dctwin-nvml-mock.conf <<'EOF'
# compat/fleet-intelligence: the agent starts after the (mock) driver is staged, as on a GPU host
[Unit]
Wants=dctwin-nvml-mock.service
After=dctwin-nvml-mock.service
EOF
systemctl daemon-reload
systemctl enable --now fleetintd.service

# ---------------------------------------------------------------- the harness's reader of the local API
cat >/usr/local/sbin/fleetint-get <<'EOF'
#!/usr/bin/python3 -I
# fleetint-get <path>: GET <path> from fleetint's local API on its unix socket, the body on stdout
import http.client, socket, sys
class C(http.client.HTTPConnection):
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(60)
        self.sock.connect("/run/fleetint/fleetint.sock")
c = C("localhost")
c.request("GET", sys.argv[1])
r = c.getresponse()
sys.stdout.write(r.read().decode())
sys.exit(0 if r.status == 200 else 1)
EOF
chmod 0755 /usr/local/sbin/fleetint-get
echo "guest-setup: done"
