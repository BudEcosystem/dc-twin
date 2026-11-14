# NVIDIA Config Manager 1.4.1's rendered configurations

Copied unchanged from NVIDIA Config Manager (`github.com/dsx-ai-factory/nv-config-manager`),
release 1.4.1, commit `2f8bf149d2971d173f09448651b408af36338ec6`,
`packages/templates/tests/resources/expected_config/`: its own golden output, a `startup.yaml`
(NVUE, applied with `nv config replace`) and a ZTP `boot-script` for each of eight switches at
Cumulus Linux 5.13.1, 5.14.0 and 5.16.1. Apache License 2.0 (`LICENSE`, `NOTICE`).

`nvcm_test.go` checks them against the emulated NVUE (18 — DSX OS: NVIDIA Config Manager's ZTP,
D80).
