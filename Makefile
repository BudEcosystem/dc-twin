# dc-twin — build, test and the compatibility harnesses.
BIN ?= bin

.PHONY: build test test-short e2e assets compat-libredfish compat-sushy compat-dmtf compat-nico-fabric compat-tinkerbell compat-ironic compat-kai compat-aicr compat-prometheus compat-kueue compat-volcano compat-medik8s e2e-refstack e2e-linux-substrate help

build: ## build dctwin and the reference stack into $(BIN)/
	go build -o $(BIN)/dctwin ./cmd/dctwin
	go build -o $(BIN)/refstack ./cmd/refstack

test: ## every test, including the end-to-end scenario suites (about four minutes)
	go test ./...

test-short: ## unit and adapter tests; skips the long end-to-end runs
	go test -short ./...

e2e: ## the scenario suites only
	go test ./test/e2e/ -v

assets: build ## obtain the captured mockups the vendor dialects bind (MIT, libredfish at a pinned commit)
	$(BIN)/dctwin assets fetch

compat-libredfish: build ## NICo's libredfish integration suite, unmodified, against the twin (needs cargo)
	DCTWIN=$(abspath $(BIN))/dctwin compat/libredfish/run.sh

compat-sushy: build ## OpenStack Ironic's sushy client against the twin
	DCTWIN=$(abspath $(BIN))/dctwin compat/sushy/run.sh

compat-dmtf: build ## DMTF Redfish Service Validator against the twin
	DCTWIN=$(abspath $(BIN))/dctwin compat/dmtf-validator/run.sh

compat-nico-fabric: build ## NICo's libnmxc and nv-rms-client's librms against the twin's NMX-C and RMS (needs cargo and protoc)
	DCTWIN=$(abspath $(BIN))/dctwin compat/nico-fabric/run.sh

compat-tinkerbell: build ## Tinkerbell's Rufio (v0.25.0) on a real kube-apiserver against the twin (needs envtest)
	DCTWIN=$(abspath $(BIN))/dctwin compat/tinkerbell/run.sh

compat-ironic: build ## OpenStack Ironic 39.0.0, unmodified, enrolling and driving the twin's BMCs over Redfish (needs python3.11+; installs into $$WORK/venv)
	DCTWIN=$(abspath $(BIN))/dctwin compat/ironic/run.sh

compat-kai: build ## NVIDIA KAI Scheduler (v0.18.6) on a real kube-apiserver and controller-manager over the twin's Nodes (needs envtest; KCM=none skips building the controller-manager)
	DCTWIN=$(abspath $(BIN))/dctwin compat/kai/run.sh

compat-aicr: build ## NVIDIA AICR (v1.0.0) snapshot, recipe and validate against the twin's cluster view (needs envtest)
	DCTWIN=$(abspath $(BIN))/dctwin compat/aicr/run.sh

compat-prometheus: build ## Prometheus 3.15.0 scraping the twin's DCGM, AMD and Gaudi exporters, with alert rules
	DCTWIN=$(abspath $(BIN))/dctwin compat/prometheus/run.sh

compat-kueue: build ## Kueue (v0.20.2) quota, flavors and topology-aware scheduling with the real kube-scheduler over the twin's Nodes (needs envtest)
	DCTWIN=$(abspath $(BIN))/dctwin compat/kueue/run.sh

compat-volcano: build ## Volcano (v1.16.0) gangs and HyperNodes from the twin's NVLink labels (needs envtest)
	DCTWIN=$(abspath $(BIN))/dctwin compat/volcano/run.sh

compat-medik8s: build ## medik8s NHC (v0.12.0) and FAR (v0.8.0) fencing the twin's hosts with fence_ipmilan and fence_redfish (needs envtest, Docker or Podman)
	DCTWIN=$(abspath $(BIN))/dctwin compat/medik8s/run.sh

.PHONY: compat-opensm
compat-opensm: ## OpenSM and infiniband-diags (Debian trixie) on the twin's InfiniBand fabric, simulated by ibsim from the model, in a Linux container (needs Docker; cross-compiles dctwin)
	compat/opensm/run.sh

.PHONY: compat-gpu-operator
compat-gpu-operator: ## NVIDIA device plugin + GFD (v0.19.3), DRA driver (0.5.0), dcgm-exporter and NFD on Kind nodes whose GPUs are the twin's, through Mokka (v0.5.0) (needs Docker, helm, envtest; builds its own dctwin and dctwin-mokka)
	compat/gpu-operator/run.sh

.PHONY: compat-grove
compat-grove: build ## NVIDIA Grove (v0.1.0-alpha.16) and the Dynamo operator (v1.5.1) with KAI over the twin's GB300 NVL72 Nodes (needs envtest, helm; Docker or Podman for Dynamo, or DYNAMO=none)
	DCTWIN=$(abspath $(BIN))/dctwin compat/grove/run.sh

.PHONY: compat-runai
compat-runai: build ## NVIDIA Run:ai 3.0.20's cluster side (KRM v0.18.3, anyworkload-controller) with per-pool KAI shards over the twin's Nodes (needs envtest, helm)
	DCTWIN=$(abspath $(BIN))/dctwin compat/runai/run.sh

.PHONY: compat-runai-cp
compat-runai-cp: ## NVIDIA Run:ai 3.0.20's control plane (its published chart, k3s in a KVM guest) driving its cluster side over the twin's Nodes (Linux, KVM; BASE, GUEST_DIR, BIN, SRC, KUBEBUILDER_ASSETS: see compat/runai/README.md)
	compat/runai/controlplane.sh

.PHONY: compat-nvsentinel
compat-nvsentinel: build ## NVIDIA NVSentinel (v1.27.0) on a real kube-apiserver against a vm GPU server's guest kernel log, printk'd by the in-host agent's kernel module (Linux, KVM, Docker or Podman; KCM=path skips building the controller-manager; BASE=a Debian 13 genericcloud qcow2 adds the GPUs through Mokka under its DCGM gpu-health-monitor)
	DCTWIN=$(abspath $(BIN))/dctwin compat/nvsentinel/run.sh

.PHONY: compat-metal3
compat-metal3: build ## Metal3's bare-metal operator (v0.14.2, built from its tag) with Ironic 39.0.0 configured as ironic-image configures it, BareMetalHosts for the twin's Redfish and IPMI BMCs (needs envtest, python3.11+, ipmitool; KCM=path adds the controller-manager)
	DCTWIN=$(abspath $(BIN))/dctwin compat/metal3/run.sh

.PHONY: compat-maas
compat-maas: ## MAAS 3.7.3's power drivers (maas.power, from its packages) against the twin's Redfish and IPMI BMCs, in a container with the twin (needs Docker or Podman; cross-compiles dctwin)
	compat/maas/run.sh

.PHONY: compat-maas-snap
compat-maas-snap: ## MAAS itself, the 3.7 snap, in an Ubuntu 24.04 KVM guest with the twin: power query, on, off, cycle, polling and BMC loss through its API (Linux with /dev/kvm, qemu)
	compat/maas/run-snap.sh

.PHONY: compat-dpf
compat-dpf: build ## NVIDIA DOCA Platform Framework (v26.8.0, zero-trust) discovering, taking over and provisioning the twin's BlueField-3 DPUs over Redfish (Linux; needs envtest, rootless Podman; downloads a 1.7 GB BFB unless BFB=test)
	DCTWIN=$(abspath $(BIN))/dctwin compat/dpf/run.sh

.PHONY: compat-fleet-intelligence
compat-fleet-intelligence: ## NVIDIA Fleet Intelligence agent (fleetint 1.5.0 from its .deb, offline mode) in a Debian KVM guest whose GPUs are the twin's through Mokka 0.5.0, with the twin's Xid 79 and 48 through NVML and the in-host agent's kernel lines (Linux, KVM; BASE=a Debian 13 genericcloud qcow2; Podman or Docker; builds its own dctwin and dctwin-mokka)
	compat/fleet-intelligence/run.sh

e2e-refstack: ## the reference stack scenarios through the CLI
	scripts/e2e-refstack.sh

e2e-linux-substrate: ## the TAP substrate and a real NOS (FRR + ISC dhcrelay) with Kea, in privileged Linux containers (needs Docker)
	scripts/linux-substrate-e2e.sh

help:
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | sed 's/:.*## /\t/'
