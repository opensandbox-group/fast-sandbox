# Artifact manifest reference

`manifest.json` is the single metadata contract for both OCI snapfiles and native object-store artifacts. The shared Go model is `artifacts.VMManifest` in `internal/artifacts/manifest.go`; Builder, fsb-vm, Fastlet and runtime-agent use its types and validation. `schemaVersion` remains `1`; historical layouts are not supported and must be rebuilt. No `metadata.json` is produced or read.

## Core VM contract

```json
{
  "schemaVersion": 1,
  "runtime": "firecracker",
  "machine": {
    "vcpuCount": 2,
    "memoryMiB": 1024,
    "rootfsSizeMiB": 20480,
    "rootfsDriveID": "rootfs",
    "rootfsDrivePath": "rootfs.dev"
  },
  "kernel": { "version": "6.1.177" },
  "compatibility": {
    "cpuArch": "x86_64",
    "firecrackerVersion": "1.16.1",
    "cpuTemplate": "none",
    "vendor": "GenuineIntel",
    "cpuFamily": 6,
    "cpuModel": 85,
    "cpuStepping": 7
  }
}
```

CPU count is a positive integer; memory and disk capacity are positive integer MiB, recorded exactly once. The rootfs drive path is `rootfs.dev` for fsb-vm/OCI and `rootfs.img` for native snapshots, matching the relative path baked into vmstate. `rootfsDriveID` records the logical device ID. `kernel.version`, CPU architecture and a known Firecracker version are required. `cpuTemplate=none` requires a resolved vendor/family/model/stepping identity; `T2` and `T2A` use the supported Firecracker-version allowlists. Restore validates architecture and CPU compatibility; the independent CLI's `--force` only downgrades VMM version mismatches.

The fixed OCI snapshot paths are `/vmstate.bin`, `/memfile` and `/manifest.json`. Native object storage publishes `rootfs.ext4`, `vmstate.snap`, `memory.snap`, `manifest.json` and `SHA256SUMS`. VM state and memory reside in those binary files, not in the JSON document.

## Complete annotated reference

All current fields are shown below using a native/S3 checkpoint. Values are illustrative; omit optional fields that do not apply. File entries require a lowercase hex SHA-256 (without `sha256:`) and a positive logical size in bytes.

**JSONC example only:** actual `manifest.json` must be strict JSON without comments.

```jsonc
{
  "schemaVersion": 1,
  "runtime": "firecracker",
  "machine": { // Captured VM configuration.
    "vcpuCount": 2,
    "memoryMiB": 1024,
    "rootfsSizeMiB": 20480, // Virtual capacity, not compressed layer size.
    "rootfsDriveID": "rootfs",
    "rootfsDrivePath": "rootfs.img" // Native: rootfs.img; OCI: rootfs.dev.
  },
  "kernel": { // Guest kernel.
    "version": "6.1.177"
  },
  "freePageReporting": true, // Build-time guest support detection for the configured FPR device; optional.
  "compatibility": { // CPU and VMM restore constraints.
    "cpuArch": "x86_64",
    "firecrackerVersion": "1.16.1",
    "cpuTemplate": "none", // none: exact CPU match; T2/T2A: versioned allowlist.
    "vendor": "GenuineIntel",
    "cpuFamily": 6,
    "cpuModel": 85,
    "cpuStepping": 7,
    "cpuModelName": "Intel(R) Xeon(R) Platinum 8269CY CPU @ 2.60GHz", // Diagnostic only; not used for CPU matching.
    "hostKernel": "6.1.0" // Capture-host kernel, not the guest kernel.
  },
  "guestNetwork": { // Captured guest network; optional standalone, complete for Fastlet.
    "iface": "eth0",
    "mac": "02:fc:00:00:00:01",
    "ip": "172.30.0.3",
    "gateway": "172.30.0.1",
    "netmask": "255.255.255.0",
    "mtu": 1500
  },
  "lineage": { // Source image and build provenance.
    "image": "registry.example/team/base:v1",
    "imageDigest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", // Original source digest; preserved by snapshots.
    "createdAt": "2026-10-22T00:00:00Z"
  },
  "platform": { // Optional platform metadata and exported state.
    "execd": "registry.example/team/execd:v1",
    "entrypoint": ["/bin/sh", "-lc", "sleep infinity"],
    "init": "/usr/local/sbin/sandbox-init",
    "envs": {
      "APP_MODE": "production"
    },
    "egressMitmCA": { // Public CA fingerprints baked into the guest.
      "certificates": [
        {
          "certSha256": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", // SHA-256 of DER certificate.
          "spkiSha256": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", // SHA-256 of DER public-key info.
          "notAfter": "2027-10-22T00:00:00Z"
        }
      ]
    },
    "checkpoint": { // Private checkpoints only; omit for reusable templates.
      "sandboxUID": "sandbox-uid-123",
      "namespace": "tenant-a"
    },
    "stateFiles": { // Exported handler state; listed only here.
      "policy.json": {
        "sha256": "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
        "sizeBytes": 256
      },
      "vault.json": {
        "sha256": "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
        "sizeBytes": 512
      }
    }
  },
  "files": { // Native/S3 file list; omit for OCI.
    "rootfs.ext4": {
      "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "sizeBytes": 21474836480
    },
    "vmstate.snap": {
      "sha256": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
      "sizeBytes": 524288
    },
    "memory.snap": {
      "sha256": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
      "sizeBytes": 1073741824
    }
  }
}
```

OCI uses `rootfs.dev`, `vmstate.bin` and `memfile`, with no `files` list. Reusable templates omit `platform.checkpoint`; exported state appears only in `platform.stateFiles`. Standalone fsb-vm needs no `platform` fields.

## Optional shared records

`guestNetwork` describes a NIC baked into vmstate: `iface` and `mac`, plus optional `ip`, `gateway`, `netmask` and `mtu`. A standalone VM without a NIC omits it. Fastlet's static guest networking requires the complete network record. Host tap names are allocated on restore and are not persisted here.

`lineage` records optional `image`, `imageDigest` and `createdAt`. Template builds record their source and creation time. Snapshots preserve the original digest and capture compatibility, updating only the source reference for the running Sandbox. `compatibility.cpuModelName` and `hostKernel` are optional diagnostics.

`freePageReporting` records the builder's (`sandboxtemplate-builder` and `fsb-vm build`) guest support detection for its configured FPR device. `true` means the guest reported the required kernel interfaces; `false` means support was not detected, although the configured device can still be present. An absent field means the result was not recorded, as with older snapshots or producers that do not report it. Snapshot and checkpoint manifests inherit this value, and `RootfsOnly` cold boots attach the FPR balloon when it is `true`. It is build provenance, not a runtime switch, proof of successful reclamation, or an admission condition; Firecracker's saved device state remains authoritative on Full restores.

## Platform extension

The entire `platform` object is optional for independent fsb-vm operation. It contains:

- `execd`, `entrypoint`, `init` and literal `envs`: build provenance. Do not put secrets in these published fields.
- `egressMitmCA`: optional public certificate fingerprints (`certificates[].certSha256`, `spkiSha256`, `notAfter`).
- `checkpoint`: `{ "sandboxUID": "...", "namespace": "..." }` for a private checkpoint. Reusable template snapshots omit inherited ownership. OCI restores also cross-check the Registry owner annotations resolved by fsb-vm.
- `stateFiles`: entries for `policy.json` and `vault.json`, each containing `sha256` and `sizeBytes`. Each state file is described exactly once here; there is no separate `egressVault` marker. Fastlet verifies size and digest before handing the opaque bytes to handlers. Native downloads use the same entries to fetch state files.

OCI `manifest.json` does not contain a S3-style VM file list: Registry manifests describe the image layers. Native `files` lists only `rootfs.ext4`, `vmstate.snap` and `memory.snap`, with `sha256` and `sizeBytes`. Native state files appear only under `platform.stateFiles`. The native image index points at this manifest and its byte digest; artifacts upload before the manifest, and the image index uploads last.

## Independent build and additional input

Without additional input, `fsb-vm build` generates the full core VM contract and optional source lineage itself. It does not need Kubernetes or Fastlet.

```bash
fsb-vm build --source registry.example/base@sha256:... \
  --output template:build --kernel /opt/kernels/vmlinux-6.1.177
```

`--manifest-extra-file` accepts an optional JSON object containing only `guestNetwork`, `lineage` and `platform`. It supplements the generated document rather than replacing it. Core keys such as `schemaVersion`, `runtime`, `machine`, `kernel` and `compatibility` are rejected. Baked NIC identity, source digest and creation time cannot be changed by supplemental input. Optional source `lineage.image` may retain the original image tag while its digest remains pinned.

```json
{
  "guestNetwork": {
    "ip": "172.30.0.3",
    "gateway": "172.30.0.1",
    "netmask": "255.255.255.0",
    "mtu": 1500
  },
  "platform": { "execd": "registry.example/execd:v1" }
}
```

The guest network supplement requires a NIC configured by build's `--tap-name` and `--guest-mac`. Producers write the final manifest before sealing snapfiles. `pack` validates machine sizes and preserves the complete input manifest, including unknown optional extensions; snapshot updates preserve unknown metadata while replacing this capture's state-file entries and ownership.

## Version policy

Readers explicitly accept only `schemaVersion: 1`. Missing or unsupported versions fail before admission. New ignorable optional fields may retain the version; changing required fields, units, types or restore semantics requires a future version and an explicit decoder. Unknown optional metadata is carried through repackaging; it must not change required restore behavior. Core and platform metadata share the same version.

Deploy matching Builder/fsb-vm/Fastlet/runtime-agent versions and rebuild existing OCI and S3 templates before using this format. Existing snapshots using the historical structure must also be recreated; changing their version number alone does not convert them. This change deliberately provides no legacy-format decoder.
