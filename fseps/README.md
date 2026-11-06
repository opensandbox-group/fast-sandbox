---
title: Fast Sandbox Enhancement Proposals
description: Proposal index and contribution entry point for Fast Sandbox enhancements.
---

# Fast Sandbox Enhancement Proposals

Fast Sandbox Enhancement Proposals (FSEPs) document major features, architectural changes, and public interface changes. Start with a short Issue discussion and write a full proposal when needed, focusing on compatibility, testing, and rollback.

See the [contributing guide](CONTRIBUTING.md) for scope, creation instructions, and the review process.

## Proposal index

Add an entry in the same PR as a new proposal; when its content or status changes, update both this index and the proposal's `last-updated` field.

| FSEP | Title | Status | Last updated |
|:----:|-------|--------|:------------:|
| [FSEP-0001](0001-rootfs-only-snapshots-overlaybd.md) | Rootfs-Only Snapshots on the OverlayBD Path | draft | 2026-11-06 |

FSEP-0001 updated on 2026-11-06: recommends optional-RAM snapshot/create with Guest freeze and VM Pause, reusing Full's upper-capture ordering without requiring a new backend barrier service. The draft covers Guest helper delivery, late-operation recovery, VMM error handling and release ownership, plus the implementation obligations of bypassing snapshot/create. Status remains draft.
