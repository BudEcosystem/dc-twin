#!/bin/sh
# gpu-guest-setup.sh install|configure|monitor <dir>: run as root inside the Debian guest lib/gpu.sh boots, the
# GPU side of the node NVSentinel watches: GPUs that are the twin's, as NVML presents them through
# NVIDIA's Mokka, under NVSentinel's DCGM-based gpu-health-monitor.
#
#   install    once per prepared image: Mokka's node agent and its mock NVML (from the released image,
#              as compat/fleet-intelligence installs them), Docker, the in-host agent's kernel module
#              (dctwin-kmsg, built for this guest's kernel by internal/inhost/kmod/guest-load.sh and
#              loaded on every boot), and NVSentinel's gpu-health-monitor image built from the source
#              at the tag (its own Dockerfile: the monitor's wheel on NVIDIA's DCGM image,
#              nvcr.io/nvidia/cloud-native/dcgm).
#   configure  every run: the GPU profile dctwin-mokka rendered from the model, then Mokka's node
#              agent (re)started on it, and enabled, so that it comes back on every boot as a
#              driver does. It stages the driver tree under /var/lib/nvml-mock/driver (the mock
#              libnvidia-ml.so.<driver version>, the /dev/nvidia* nodes, the engine's config.yaml and
#              the runtime overrides.yaml beside it) and bind-mounts it on /run/nvidia/driver. The
#              mock finds both documents from the path it was loaded from, so every process must load
#              the tree's copy: its lib64 is first on the linker's path.
#   monitor    after every boot: the gpu-health-monitor container, with the chart's command and arguments
#              (ghm.args, rendered from the chart by run.sh), its ConfigMap's files (dcgmhealth/) and
#              an in-process DCGM host engine (--dcgm-mode local-managed, the chart's embedded-mode).
#              The chart runs it privileged under the NVIDIA runtime class, which gives the container
#              the driver's libraries; here the driver tree is mounted and its lib64 put on
#              LD_LIBRARY_PATH instead. Its /var/run is the node's /run/nvsentinel, as the chart's
#              hostPath is, where the node's platform-connectors socket appears (run.sh forwards it
#              there over ssh, from the one platform-connectors the node runs).
#
# <dir> holds what lib/gpu.sh staged: mokka/bin/{node-agent,nvml-mock-ctl,nvidia-smi},
# mokka/lib/{libnvidia-ml,libcuda}.so.<ver>, kmod/{Kbuild,dctwin_kmsg.c,guest-load.sh},
# gpu-health-monitor.tar (the monitor's source), build.env; for configure config.yaml (the rendered
# profile) and guest.env (NODE_NAME, GPU_COUNT, DRIVER_VERSION); for monitor dcgmhealth/ and ghm.args.
set -eu
mode=$1 D=$2
case "$mode" in
install)
	export DEBIAN_FRONTEND=noninteractive
	apt-get update -qq
	apt-get install -y -qq --no-install-recommends docker.io docker-buildx ca-certificates >/dev/null
	# Debian 13 ships the client apart from the daemon, as a recommendation of docker.io
	command -v docker >/dev/null || apt-get install -y -qq --no-install-recommends docker-cli >/dev/null
	systemctl enable --now docker >/dev/null 2>&1
	install -m 0755 "$D/mokka/bin/node-agent" "$D/mokka/bin/nvml-mock-ctl" "$D/mokka/bin/nvidia-smi" /usr/local/bin/
	# the node agent stages /usr/local/lib/{libnvidia-ml,libcuda}.so.*.*.* (its image's path) into the driver tree
	install -m 0644 "$D"/mokka/lib/lib*.so.*.*.* /usr/local/lib/
	cat >/etc/systemd/system/dctwin-nvml-mock.service <<'EOF'
# compat/nvsentinel: Mokka's node agent on this host, as the nvml-mock DaemonSet runs it on a node
[Unit]
Description=Mokka nvml-mock node agent (GPUs rendered from the dc-twin model)
After=local-fs.target
Before=docker.service

[Service]
EnvironmentFile=/etc/nvml-mock/guest.env
Environment=MOCK_NVML_KMSG=
ExecStart=/usr/local/bin/node-agent start --config=/etc/nvml-mock/config.yaml --host-root=/ --health-addr=127.0.0.1:9091 --log-format=plain --shutdown-timeout=5s --kernel-log=
ExecStartPost=/bin/sh -c 'for i in $(seq 1 120); do [ -e /var/lib/nvml-mock/driver/usr/lib64/libnvidia-ml.so.1 ] && [ -e /run/nvidia/driver/usr/lib64/libnvidia-ml.so.1 ] && exit 0; sleep 0.5; done; exit 1'
Restart=on-failure

[Install]
WantedBy=multi-user.target
EOF
	echo /var/lib/nvml-mock/driver/usr/lib64 >/etc/ld.so.conf.d/00-nvml-mock.conf
	# the node's /run/nvsentinel (the chart's hostPath), where the forwarded socket is bound by the ssh user
	echo 'd /run/nvsentinel 0755 debian debian -' >/etc/tmpfiles.d/dctwin-nvsentinel.conf
	# the kernel module kmsg-relay.py writes through: loaded now and on every boot
	sh "$D/kmod/guest-load.sh" "$D/kmod" | tee /var/lib/dctwin-kmod.txt | sed 's/^/gpu-guest-setup: kernel module dctwin-kmsg: /'
	# NVSentinel's gpu-health-monitor from the tag's source, by its own Dockerfile (BuildKit), as its
	# Makefile's docker-build-dcgm4 builds it (the Dockerfile's default DCGM, 4.7.0-ubuntu24.04)
	rm -rf /root/nvs && mkdir -p /root/nvs && tar -C /root/nvs -xf "$D/gpu-health-monitor.tar"
	. "$D/build.env"
	DOCKER_BUILDKIT=1 docker build -q --network=host -t "$GHM_IMAGE" --build-arg VERSION="$NVS_REF" --build-arg GIT_COMMIT="$NVS_COMMIT" \
		-f /root/nvs/health-monitors/gpu-health-monitor/Dockerfile /root/nvs >/dev/null
	docker image inspect "$GHM_IMAGE" --format 'gpu-health-monitor image {{.Id}} ({{.Size}} bytes)'
	docker builder prune -af >/dev/null 2>&1 || true
	rm -rf /root/nvs
	echo "gpu-guest-setup: installed"
	;;
configure)
	. "$D/guest.env"
	install -D -m 0644 "$D/config.yaml" /etc/nvml-mock/config.yaml
	install -D -m 0644 "$D/guest.env" /etc/nvml-mock/guest.env
	systemctl daemon-reload
	systemctl enable -q dctwin-nvml-mock.service
	systemctl restart dctwin-nvml-mock.service
	for l in libnvidia-ml.so.1 libcuda.so.1; do
		[ -e "/var/lib/nvml-mock/driver/usr/lib64/$l" ] && ln -sfn "/var/lib/nvml-mock/driver/usr/lib64/$l" "/usr/lib/x86_64-linux-gnu/$l"
	done
	ldconfig
	nvml=$(python3 -I -c 'import ctypes; ctypes.CDLL("libnvidia-ml.so.1"); print(next((l.split()[-1] for l in open("/proc/self/maps") if "libnvidia-ml" in l), ""))')
	case "$nvml" in
	/var/lib/nvml-mock/driver/*) ;;
	*) echo "libnvidia-ml.so.1 resolves to $nvml, not the driver tree's copy: the mock would not see its overrides" >&2; exit 1 ;;
	esac
	echo "gpu-guest-setup: configured, NVML $nvml, kernel module $(cat /var/lib/dctwin-kmod.txt 2>/dev/null)"
	;;
monitor)
	. "$D/guest.env"
	. "$D/build.env"
	install -d -m 0755 -o debian -g debian /run/nvsentinel
	mkdir -p /etc/dcgmhealth /var/lib/nvsentinel
	cp "$D/dcgmhealth/"* /etc/dcgmhealth/
	set --
	while IFS= read -r a; do set -- "$@" "$a"; done <"$D/ghm.args"
	docker rm -f dctwin-nvs-gpu-health-monitor >/dev/null 2>&1 || true
	docker run -d --name dctwin-nvs-gpu-health-monitor --privileged --network host \
		-v /var/lib/nvml-mock/driver:/var/lib/nvml-mock/driver -v /run/nvsentinel:/var/run \
		-v /etc/dcgmhealth:/etc/dcgmhealth -v /var/lib/nvsentinel:/var/lib/nvsentinel:ro \
		-e LD_LIBRARY_PATH=/var/lib/nvml-mock/driver/usr/lib64 -e NODE_NAME="$NODE_NAME" \
		--entrypoint gpu_health_monitor "$GHM_IMAGE" "$@" >/dev/null
	echo "gpu-guest-setup: monitor started"
	;;
*) echo "usage: gpu-guest-setup.sh install|configure|monitor <dir>" >&2; exit 2 ;;
esac
