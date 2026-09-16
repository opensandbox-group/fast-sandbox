// Package agent implements the node-level firecracker-runtime pull
// layer of the Firecracker on-demand loading design: the consumer side of
// the addressing chain SandboxSpec.Image -> index -> manifest ->
// content-addressed native artifacts, materialized in the cache shared with
// the driver (<StateRoot>/images/<sha256(image)>/).
package agent
