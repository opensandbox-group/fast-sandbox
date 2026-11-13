// Package fastletauth authenticates control-plane callers of the Fastlet
// control API with short-lived, Kubernetes-managed ServiceAccount tokens.
//
// The control plane (Fast-Path and Reconcilers) presents an audience-bound,
// expiring bearer token minted through the TokenRequest API for its own
// ServiceAccount (see TokenRequestSource): Kubernetes signs, bounds, and
// rotates the credential, so the deployment carries no auth key material and
// no operator-managed secret.
//
// The Fastlet validates the token locally the same way the API server
// validates ServiceAccount tokens: it fetches the issuer's OIDC discovery
// document and JWKS once, caches the signing keys, and verifies the JWT
// signature, expiry, issuer, audience, and ServiceAccount subject on every
// request with no per-request network round trip. Unknown signing keys
// trigger a JWKS refresh, so key rotation by the API server is followed
// automatically.
//
// Enforcement is fail-closed: a request without a valid, audience-matched
// token issued to an allow-listed ServiceAccount is rejected. Heartbeat and
// health endpoints are exempt because they expose no sensitive intent, and
// metrics stay scrape-only.
package fastletauth
