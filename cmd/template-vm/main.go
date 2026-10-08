// Command template-vm restores a running Firecracker microVM on the local
// host from a template stored in a remote OCI registry.
//
// A template is a pair of overlaybd images
// (${repo}:${template_id}_rootfs and ${repo}:${template_id}_snapfiles).
// The tool pulls their manifests, generates overlaybd config.v1.json files,
// attaches the block devices through overlaybd-ublkd, mounts the snapfiles
// device, and resumes the VM from the in-disk snapshot (vmstate.bin +
// memfile) via the Firecracker snapshot/load API.
//
// It is a single-host data-plane tool: it bypasses every fast-sandbox
// control plane (no fastpath/CRD/reconciler) and does not reuse the fastpath
// gRPC stack. See docs/design/adr-template-vm-cli.en.md.
//
// The implementation is organized per concern, mirroring the ADR:
//
//	config.go    — global flags and shared configuration
//	template.go  — template.json input manifest
//	metadata.go  — snapfiles metadata.json (authoritative VM spec) + validation
//	registry.go  — pull overlaybd manifests with go-containerregistry
//	cred.go       — merge dockerAuth into overlaybd's global cred.json
//	overlaybd.go — config.v1.json generation and the hybrid writable upper
//	ublkd.go     — overlaybd-ublkd HTTP-over-unix-socket control client
//	firecracker.go — minimal Firecracker REST client + process launch
//	network.go   — NetworkProvider abstraction (MVP: none)
//	state.go     — per-sandbox state directory layout and state.json
//	create.go / list.go / delete.go — the command surface
package main

func main() {
	Execute()
}
