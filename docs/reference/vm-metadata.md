# VM discovery metadata (`fast-sandbox.json`)

When provisioning a VM, the Firecracker driver writes a small, immutable metadata record next to the VM's process log (`firecracker.log`). Node-local agents on the node use it to map a VM directory back to its Sandbox identity with a local directory scan — without watching Sandbox CRs cluster-wide. The jail directory name only carries the first 32 characters of the Sandbox UID (firecracker `--id` limit), so this record is the only node-local source of the full identity.

## Where to find it

The file name is always `fast-sandbox.json`, inside the VM directory. In both modes the record sits next to `firecracker.log`:

| Mode | Path |
|------|------|
| Jailer | `<StateRoot>/jails/firecracker/<uid[:32]>/root/fast-sandbox.json` |
| Direct | `<StateRoot>/sandboxes/<sandbox-uid>/fast-sandbox.json` |

`<StateRoot>` is the Firecracker driver state root (built-in default `/var/lib/fast-sandbox/firecracker`).

## File structure

```json
{
  "version": 1,
  "uid": "6f9c1e8a-3b2d-4c5e-8f70-a1b2c3d4e5f6",
  "namespace": "team-a",
  "name": "sbx-7d9f2c",
  "placementAttempt": 2
}
```

| Field | Type | Description |
|-------|------|-------------|
| `version` | int | Schema version; currently `1` |
| `uid` | string | Full (36-char) Sandbox CR UID |
| `namespace` | string | Sandbox namespace |
| `name` | string | Sandbox name |
| `placementAttempt` | int64 | Optional; `PlacementStatus.Attempt` of the placement that created this VM. Disambiguates a stale jail from its replacement when a sandbox moves back to the same node before GC. Absent when zero. |

## How to read it (consumer contract)

1. Scan the VM directories (jails under `<StateRoot>/jails/*/*/root/`,
   sandboxes under `<StateRoot>/sandboxes/*/`).
2. Read `fast-sandbox.json` and parse the JSON.
3. A missing or unparseable file means **not ready yet**: skip the directory
   and retry on the next periodic scan. This is never fatal and never
   blacklists the VM. Publication is asynchronous: the record's file is
   created before provisioning returns and the payload is written shortly
   after, so an empty or short JSON prefix is possible by design (single
   `write()` without temp-file rename) and recovers on a later scan once the
   payload lands.
4. A parse failure that persists across scans is a **permanently unusable
   record**: the payload write failed or was interrupted (for example by a
   process restart or a full disk), and there is no write tick or automatic
   repair. This is accepted — that VM's console cannot be collected until
   teardown; the VM itself is unaffected. Consumers may give up collecting
   such a directory after a bounded number of retries.
5. A directory disappearing is the **stream end** signal; keep a grace window
   so a sandbox that moves back to the node resumes its stream instead of
   finalizing.
6. `placementAttempt` lets you tell a stale jail from the new one when both
   exist for the same truncated UID prefix.

## Writer semantics (reference)

The driver composes the JSON in memory, creates the record file on the create
path (`O_EXCL`, write-once: an existing record is never rewritten), and
writes the payload with a single `write()` from a background goroutine — no
fsync, no temp-file rename. The payload write never blocks or fails
provisioning; a create or write error (e.g. disk full) is logged, and that
VM's console is not collectable. A failed or interrupted write can leave an
empty or short record that never heals (see the consumer contract above).

Teardown removes the whole jail/sandbox directory with a recursive
`RemoveAll`. This is **not atomic**: individual entries are unlinked one by
one, so a consumer racing teardown can successfully read the metadata and
still get `ENOENT` when it opens the log, or observe the metadata file
disappear between its scan and its read. Consumers must tolerate any entry
vanishing between scan, metadata read, and log open, and treat it as the
stream end — the writer makes no atomic-visibility promise.
