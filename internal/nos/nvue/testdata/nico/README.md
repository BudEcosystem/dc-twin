# NICo's expected NVUE configurations

These are NICo's own test oracles for its DPU agent's NVUE templates, copied unmodified from
`crates/agent/templates/tests/` of NVIDIA Infra Controller (`dsx-ai-factory/infra-controller`) at
`02b92f18fed776c8ef0056803338707c5e6bcb6b`, Apache-2.0 (see that repository's LICENSE).
`hbn_test.go` applies each through the twin's NVUE as NICo's agent would (D71): every one must
validate against the emulated schema and render to ifupdown2, FRR and nftables.
