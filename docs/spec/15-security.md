# 15 — Security

Draft 0.1 asserted that the twin is unreachable from the software under test (D10) and said
nothing about how, or about who may use the control API ([R-17](../reviews/2026-10-18-spec-review.md),
[R-53](../reviews/2026-10-18-spec-review.md)). This document is the mechanism, in force from M0
([D29](11-decisions.md#d29--security-baseline-from-m0)).

## Threat model

Two adversaries:

1. **The software under test.** It must not be able to read the model, the scenario, the event log
   or the console, and must not be able to tell a `model` device from a `vm` device by anything a
   lifecycle manager is known to key on (ServiceRoot identity, system Id, DHCP vendor class, IPMI
   manufacturer id). It *may* be able to tell by TCP fingerprints, timing and certificate chains;
   the twin does not claim otherwise.
2. **Users of a shared twin.** A viewer must not act; an operator must not change the estate;
   nobody reads another's credentials.

Out of scope: a malicious pack (packs are code the operator chooses to run, like a plugin), and an
adversary with root on the twin's host.

## Network isolation (D10)

| Rule | Mechanism |
|---|---|
| The control zone is on no twin network | The engine, control API, console, scenario runner and secret store listen only on a unix socket and a loopback address inside the **control network namespace**. Twin bridges are created with no host IP address and `forwarding` off. In a Kubernetes cluster the operator may serve the API on the pod's address for an Ingress (`--allow-remote-control`, [D54](11-decisions.md)); the chart's NetworkPolicy then admits only the Ingress controller to it |
| Adapters are dual-homed but cannot forward | Each adapter runs in its own namespace with one interface per twin network it serves and one unix socket (or vsock) to the engine; it has no route between them, and nftables in the host namespace drops any packet from a twin bridge to the host's addresses or to the control namespace |
| Observers originate traffic; the stack cannot answer back | Observers ([02](02-architecture.md#observers)), accessor reads and the console's inspector reach the service zone and NOS through a **stateful one-way filter**: connections may be initiated from the control side only, and return traffic is allowed only for those connections |
| The install path blackholes the control address | The service zone pulls images and charts from the twin's **artifact mirror**, a registry and HTTP cache on the `service` network whose only upstream is the operator's configured mirror; it never forwards to the control namespace ([The install path](#the-install-path)) |
| The hardware bridge imports nothing but the device | The bridged physical interface is attached with `isolated` on, DHCP OFFER/ACK from the physical side is dropped, and only frames from the configured device MAC(s) are accepted |
| The in-host agent cannot be reached from the guest network | It listens on virtio-vsock only; the guest's network interfaces carry no route to it |
| CI proves it | A test scans the control address and the unix socket path from a service-zone VM, from an adapter namespace and from a `vm` guest, and must find nothing ([16](16-testing.md)) |

## The install path

A stack's `install` steps run in the service zone against the **artifact mirror**:

- images and charts are pulled by digest from the mirror, which was populated from the operator's
  upstream before the run (`dctwin mirror pull <stack>`), so a run needs no internet;
- the mirror is a device of the twin like any other (`class: appliance`, `role: service`), so the
  stack sees a registry as a site would;
- what the mirror serves is recorded in the run's fidelity report.

## Users and the control API

| Mode | Authentication | Who |
|---|---|---|
| Local (default) | The unix socket's file mode, plus a per-estate bearer token in `~/.dctwin/<estate>/token` for the loopback listener | The one user who started the engine |
| Remote ([D54](11-decisions.md)) | The estate token, which the operator keeps (`--token-file`), on an API served beyond loopback; the console sends it only in a header | A team on one twin in a cluster, until OIDC |
| Shared | OIDC; the console and CLI obtain tokens from the configured issuer | A team on one twin |

Roles and the operations each allows:

| Operation group ([09 — Control API](09-runtime.md#control-api)) | viewer | operator | designer |
|---|---|---|---|
| Model, Catalogues, Events (read), Inspector (read), Scenarios (read) | ✓ | ✓ | ✓ |
| Acts, Clock, Scenarios (run), Consoles, Sessions | | ✓ | ✓ |
| Drafts (create, validate, diff, apply), hardware credentials | | | ✓ |

Every act carries the principal in the event's `source`. Tokens are never written to the event log.

## Secrets

- A **secret store** in the control namespace (a file encrypted with a key in the OS keychain or
  an operator-supplied KMS) holds: the estate CA's private key (TLS and swtpm EK issuance), BMC
  factory credentials the twin generated, credentials the stack later set (so the twin can verify
  a rotation happened), accessor credentials the twin issued to itself, and `hardware` device
  credentials supplied at run time.
- The estate file holds **references** (`credentialsRef`), never values.
- The model-store snapshot and the event log hold **no secret**: request bodies are redacted by
  field name (`Password`, `Token`, `PrivateKey`, `Secret`, `Authorization`) and by dialect
  annotation before they are committed ([D27](11-decisions.md#d27--committed-and-derived-state-event-retention-and-redaction)).
- Exported scenarios and fidelity reports are produced from the redacted log.

## The twin's own certificates

The estate CA is created at `dctwin up`, stored in the secret store, and issues: server
certificates for every emulated BMC (subject = the device's serial), the swtpm EK certificates,
and the artifact mirror's certificate. The stack is handed the CA certificate as a site would hand
it (`estate.tpm.ca` and `estate.pki.ca` inputs). The CA never leaves the control namespace.

## Hardware bridge host requirements

The operator attaches a dedicated physical interface (no IP address, no other bridge membership,
no DHCP client). The twin refuses an interface that carries an address or is the host's default
route.
