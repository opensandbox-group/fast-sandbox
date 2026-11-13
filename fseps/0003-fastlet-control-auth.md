---
title: "Fastlet Control Authentication"
authors:
  - "@Pangjiping"
creation-date: 2026-11-13
last-updated: 2026-11-13
status: draft
---

# FSEP-0003: Fastlet Control Authentication

## Problem and goals

The Fastlet control API is a plain HTTP+JSON protocol served on the Pod IP at port 5758 (`internal/fastlet/server/rpc_server.go`, `internal/protocol/fastlet/fastlet_client.go`). It carries the highest-authority operations in the system — atomic Create, Delete, snapshot, drain, handler relay, and a heartbeat that reports the full local Sandbox inventory — and it performs no application-layer authentication: the server installs no auth middleware on its mux, and the client sends no credential, only trace headers. The only isolation is a NetworkPolicy that restricts port 5758 to Pods carrying the platform control-plane labels (`config/network-policy/default.yaml`). That is a network-location trust model: the identity of a caller is the network position it happens to hold.

Three gaps follow from that model. First, the policy only holds where the CNI enforces NetworkPolicy and where label selectors can be trusted; a compromised Pod in the system namespace, a forged label, or a cluster whose plugin ignores policies exposes the full lifecycle API. Second, the connection is unauthenticated and unencrypted, so nothing on the path binds a request to an identity at all. Third, the protection is asymmetric with the data plane, which already verifies per-route Ed25519 credentials fencing namespace, Sandbox UID, port, and Fastlet Pod UID (`internal/dataplane/auth/credential.go`, `internal/dataplane/fastletproxy/proxy.go`): the lower-authority plane is better defended than the higher-authority one.

This proposal makes authentication on the Fastlet control API **always enabled** with no disable switch, under these goals:

- Every control endpoint except `/livez`, `/readyz`, `/metrics`, and `/api/v2/fastlet/heartbeat` rejects callers that do not present a valid, audience-bound token issued to an allow-listed ServiceAccount. Heartbeat stays open deliberately: diagnostics tooling and the heartbeat loop rely on it and it grants no intent-changing authority.
- No operator-managed key material. The credential is a short-lived Kubernetes ServiceAccount token minted through the TokenRequest API and rotated by lifetime, in the manner Kubernetes components authenticate to each other; Kubernetes signs, bounds, and revokes it.
- Low steady-state overhead. No per-request network round trip and no public-key operation on the hot path: verification results are cached per token.
- Compatibility with the existing Fastlet security posture: Fastlet Pods run with `automountServiceAccountToken` forced off (`docs/reference/configuration.md`) and hold no Kubernetes credentials.
- Fail-closed behavior everywhere: a cluster whose RBAC has not been updated, a Fastlet without its issuer anchor, or an unreachable issuer degrades into loud errors, never into silent unauthenticated traffic.

### Non-goals

- Transport encryption (mTLS or TLS on port 5758). Caller identity is established at the application layer; wire encryption is a separate decision that can be layered later without protocol change.
- The data plane. Sandbox Proxy traffic is already credential-gated (`docs/concepts/architecture.md`), and this proposal does not alter it.
- Migrating the Fastlet control protocol to gRPC. The protocol stays HTTP+JSON; performance is not the bottleneck (runtime startup dominates Create latency) and the curl/kubectl diagnostics surface depends on plain HTTP.
- Protecting user protocols (Execd and friends). Fast Sandbox does not define them.
- A mixed-version or downgrade-tolerant wire protocol. This is an accepted breaking change; see Compatibility.

## Proposal and tradeoffs

The client half lives in the control plane; the server half lives in the Fastlet; a new `internal/fastletauth` package holds both halves plus the HTTP middleware.

**Client: TokenRequest-issued ServiceAccount tokens.** `fastletauth.TokenRequestSource` mints a token for the Pod's own ServiceAccount through the TokenRequest API with audience `fast-sandbox-fastlet` (`fastletauth.Audience`) and an expiration of one hour, caches it, and re-mints when the remaining lifetime drops below twenty minutes, using the `ExpirationTimestamp` the API server returns. The identity (namespace and ServiceAccount name) is derived from the Pod's own projected token and namespace file, with `FASTLET_AUTH_NAMESPACE` / `FASTLET_AUTH_SERVICE_ACCOUNT` env overrides for processes running outside a Pod. `FastletClient.SetTokenSource` installs the source; every request, heartbeat included, carries `Authorization: Bearer <token>`, and a token-acquisition failure fails the call closed — no request ever leaves without a credential.

**Server: local JWT verification against the issuer's published keys.** `fastletauth.Authenticator` validates tokens the same way the API server validates ServiceAccount tokens: it fetches the issuer's OIDC discovery document and JWKS once, caches the signing keys (RS256 and ES256 P-256), and verifies signature, `exp`, `nbf`, `iss`, `aud`, and `sub` locally. Unknown `kid` values trigger a JWKS refresh throttled to one attempt per thirty seconds, so API-server signing-key rotation is followed without restarts and a forged-kid flood cannot become a request storm. Positive verdicts are cached for one minute and negative verdicts for five seconds, which reduces the per-request cost to a SHA-256 and a map lookup and keeps the amortized public-key work near zero. The subject must be an exact `system:serviceaccount:<namespace>:<name>` entry in the allow list, which defaults to the platform `fast-sandbox-fastpath` and `fast-sandbox-controller` ServiceAccounts and is narrowed or extended only through `FASTLET_AUTH_ALLOWED_SUBJECTS`.

**The anchor travels with the Pod template.** Fastlet Pods hold no Kubernetes credentials, so they cannot discover the issuer themselves; the Pool Controller resolves the anchor from its own projected ServiceAccount identity at startup — issuer URL plus base64 cluster CA bundle (`fastletauth.ResolveIssuerAnchor`) — and injects `FASTLET_AUTH_ISSUER` and `FASTLET_AUTH_CA_BUNDLE` as platform-owned env into every Fastlet Pod it materializes. The injection is part of the Pod template hash, so it participates in the normal Pool rolling update. The issuer URL is the trust anchor and never comes from an incoming token, so an attacker cannot redirect key material to a host they control.

**Enforcement mapping.** `fastletauth.Middleware` wraps the server mux: exempt paths pass through; a missing or invalid token yields 401 with `WWW-Authenticate`; a valid token from a non-allow-listed subject yields 403; a validator failure (issuer unreachable) yields 503 so callers retry instead of treating the call as rejected. The authenticated subject is attached to the request context.

**Considered alternatives.**

- *TokenReview validation* (the literal webhook pattern) was rejected: it couples every cold verdict to an API-server round trip, requires `tokenreviews/create` RBAC bound to the deployer-controlled Fastlet ServiceAccount (or a cluster-scope `system:auth-delegator` binding provisioned by the Pool Controller), and pulls client-go into the deliberately node-local Fastlet binary. Local JWKS verification has the same trust root with none of that, and it is what the API server itself does with ServiceAccount tokens.
- *Operator-provisioned shared keys* (the route-keys Secret pattern) were rejected because the credential would be long-lived and hand-distributed, which is exactly what this proposal removes.
- *Ephemeral per-replica signing keys distributed at Pod creation* were rejected: Fastlet Pods cannot track a key set that changes on every control-plane restart, so rolling updates would brownout the control path.
- *mTLS* was rejected for this scope: certificate distribution is operator configuration, and by itself it answers "which Pod spoke TLS" without answering "is this ServiceAccount allowed to mutate Sandboxes".

**Risks.** A managed cluster whose `--service-account-issuer` URL is not reachable from Pods fails verification with 503s; the standard in-cluster issuer (`kubernetes.default.svc`) is reachable and is the documented default. A deleted ServiceAccount's already-minted tokens stay valid until expiry — bounded by the one-hour TTL, the same window a cached TokenReview verdict would have. Both are recorded as accepted limitations rather than open questions.

## Compatibility

This is an accepted breaking change at the wire level; there is no opt-out and no protocol negotiation.

- **Supported combinations.** Control-plane components and Fastlets must both be this version or newer. New control plane against an old Fastlet works: the old server ignores the `Authorization` header, and the interim state is the previous (unauthenticated) behavior, not an outage. Old control plane against a new Fastlet fails closed with 401 on every lifecycle call: loud, and safe in the security direction.
- **Upgrade order.** Apply the updated RBAC first, then roll the control plane (Fast-Path and controller Deployments), then let the Pool Controller roll Fastlet Pods. This order never crosses a broken combination.
- **Rollback order.** Fastlets back first, then the control plane. The reverse order crosses old-control-plane + new-Fastlet and 401s the control path until both sides are back.
- **RBAC.** `config/rbac/base.yaml` adds `create` on `serviceaccounts/token` to the `fast-sandbox-controller` and `fast-sandbox-fastpath` ClusterRoles, fenced by `resourceNames` to each role's own ServiceAccount, so a compromised component cannot mint tokens for arbitrary identities. A cluster without the grant fails at the first Fastlet call with the underlying Forbidden error visible in the wrapped message.
- **Configuration surface.** No CRD, gRPC API, SDK, or CLI changes. New environment variables are platform-owned (`FASTLET_AUTH_ISSUER`, `FASTLET_AUTH_CA_BUNDLE`) or deployment-tunable overrides (`FASTLET_AUTH_ALLOWED_SUBJECTS`, `FASTLET_AUTH_AUDIENCE`, `FASTLET_AUTH_NAMESPACE`, `FASTLET_AUTH_SERVICE_ACCOUNT`), documented in `docs/reference/configuration.md`. The injected anchor changes the Fastlet Pod template hash, so upgrading rolls every Fastlet Pool; live Sandboxes on those Pods end according to the platform's standard Fastlet Pod replacement semantics, exactly as with any Fastlet version bump.
- **Kubernetes version floor.** The design uses three mechanisms that settled at Kubernetes v1.22: the GA TokenRequest API (`authentication.k8s.io/v1` with the `serviceaccounts/token` subresource, audience scoping, and `expirationSeconds`), bound projected ServiceAccount tokens (the control plane reads its own projected token to anchor the issuer), and the API server's OIDC discovery and JWKS endpoints. RBAC `resourceNames` predates that floor. No feature gates or non-default admission plugins are required, and the verifier accepts both RSA (RS256) and EC P-256 (ES256) ServiceAccount signing keys, so clusters using either key type work unchanged. The practical floor is v1.24: legacy never-expiring ServiceAccount tokens were removed there, which is the token shape this design assumes; v1.20 and v1.21 only expose a beta TokenRequest that current client-go no longer speaks. The repository's own integration and performance environments run v1.27+, and current managed distributions (ACK/GKE/EKS) ship v1.24 or newer by default.
- **Local development.** A host-process controller can still authenticate by exporting `FASTLET_AUTH_NAMESPACE`, `FASTLET_AUTH_SERVICE_ACCOUNT`, and an identity whose RBAC allows minting its own tokens; without them the controller exits at startup with a directed error. There is no unauthenticated mode to fall back to.

## Test plan

Unit tests cover the real verification paths against a local OIDC issuer harness (`internal/fastletauth/fastletauthtest`) that serves discovery and JWKS and mints signed tokens:

- Acceptance of RS256 and ES256 P-256 tokens; rejection of empty, malformed, and over-segmented tokens, wrong audience, wrong issuer, expired and not-yet-valid tokens, bad signatures, unknown `kid`, unsupported algorithms, and payloads without `exp`.
- Allow-list enforcement (403 class) and verdict caching: a cached positive verdict survives an allow-list change until the TTL elapses; unknown-`kid` refreshes are throttled; key rotation is followed after publication.
- `TokenRequestSource`: audience and expiration propagation, mint-once-then-reuse, refresh past the margin, re-mint when the granted lifetime sits inside the margin, and failure wrapping as `ErrUnavailable` with the RBAC cause visible.
- Middleware: 401/403/503 mapping, subject propagation, exempt-path passthrough (`/livez`, `/readyz`, `/metrics`, heartbeat), and enforcement on every other API path.
- Wiring: `FastletClient` attaches the header to every call and fails closed when minting fails without sending a request; `FastletServer.Handler` enforces authentication end to end and keeps the heartbeat open; the Pool Controller injects the anchor env into the Fastlet container and never reintroduces a token mount.

Acceptance criteria for the repository toolchain: `make test SCOPE=unit` passes on Linux for all affected packages (`internal/fastletauth/...`, `internal/protocol/fastlet`, `internal/fastlet/server`, `internal/controlplane/reconciler`, `internal/controlplane/{fastpath,orchestrator,fastletcontrol}`); the pre-existing darwin-only failures in `cmd/fsb-vm`, `cmd/sandboxtemplate-builder`, `internal/dataplane/fastletproxy`, `internal/fastlet/{infra,sandbox}`, and `internal/runtime/firecracker**` are unchanged.

Integration validation in the Firecracker integration environment: with everything in-cluster and the updated RBAC applied, `fastctl` Sandbox create, pause/resume, snapshot, and pool scaling work unchanged; `curl http://127.0.0.1:5758/api/v2/fastlet/create` without a token returns 401 while `curl .../api/v2/fastlet/heartbeat` returns 200; deleting the `serviceaccounts/token` grant produces Fast-Path errors naming the missing permission rather than traffic to Fastlets; restarting a Fast-Path Pod re-mints tokens and traffic continues without manual action.

## Rollout and rollback

The feature is always on; rollout is an ordered upgrade, not a staged flag flip. Deploy the RBAC change, then the control-plane Deployments (which begin minting tokens and resolving the anchor), then Fastlet Pools, whose Pod templates pick up the injected anchor and roll through the normal Pool lifecycle. Signals to watch: the `Fastlet control authentication enabled` log on every control-plane Pod and Fastlet; Fastlet-side 401/403 counts surfaced through the existing HTTP status instrumentation; control-plane errors whose cause chain contains the TokenRequest failure. A missing RBAC grant or an absent anchor produces explicit errors, never silent fallback.

Rollback reverts images in the reverse order — Fastlet Pools first, then the control plane — because a new Fastlet rejects the unauthenticated old control plane while an old Fastlet merely ignores the header. There is no rollback to "this version with authentication off"; the only unauthenticated state is the previous version pair, which is the status quo this proposal replaces. No persisted resources, snapshots, or CRD objects are affected by rollback; the transient cost on either transition is the standard Fastlet Pod replacement effect on live Sandboxes.

## Related links

- Implementation: `internal/fastletauth/`, `internal/fastletauth/fastletauthtest/`, `internal/protocol/fastlet/fastlet_client.go`, `internal/fastlet/server/rpc_server.go`, `cmd/controller/main.go`, `cmd/fastlet/main.go`, `internal/controlplane/reconciler/sandboxpool_controller.go`, `config/rbac/base.yaml`.
- Documentation: [Fastlet configuration](../docs/reference/configuration.md) (Control authentication), [Architecture](../docs/concepts/architecture.md).
- Related mechanism: data-plane route credentials in `internal/dataplane/auth` (FSEP-track for snapshots: [FSEP-0001](0001-rootfs-only-snapshots-overlaybd.md)).
