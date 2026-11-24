// Package fastletauth authenticates control-plane callers of the Fastlet
// control API with short-lived, Kubernetes-managed ServiceAccount tokens.
//
// The control plane presents an audience-bound, expiring bearer token minted
// through the TokenRequest API for its own ServiceAccount: Kubernetes signs,
// bounds, and rotates the credential, so deployments carry no auth key
// material. The Fastlet validates the token locally the way the API server
// validates ServiceAccount tokens — OIDC discovery plus a cached JWKS, no
// per-request network round trip — and rejects callers whose ServiceAccount
// is not allow-listed.
//
// Enforcement is fail-closed. Heartbeat and health endpoints are exempt
// because they grant no intent-changing authority.
package fastletauth
