// Package firecracker-runtime is the node-level runtime agent entry
// point: a thin assembly of the pull client, the durable lease state, the
// UDS management server, and (when FAST_SANDBOX_NODE_NAME is set) the
// node-readiness manager — host checks, Firecracker asset installation,
// and the scheduling labels + FirecrackerReady condition that gate fastlet
// scheduling (the kata-deploy pattern). Deployed as a DaemonSet.
package main
