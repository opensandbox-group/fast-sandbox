# fastctl reference

`fastctl` is the Fast Sandbox command-line client. It owns lifecycle commands, platform diagnostics, route resolution, and hand-off to upstream Infra Component SDKs.

## Build

```bash
make build COMPONENT=fastctl
```

The binary is written to `bin/fastctl`.

## Global flags

| Flag | Default | Meaning |
|---|---|---|
| `--config` | `./.fastctl/config.json` | Explicit configuration file |
| `--endpoint` | `localhost:9090` | Fast-Path gRPC endpoint |
| `--namespace`, `-n` | `default` | Kubernetes namespace |
| `--proxy-endpoint` | resolved by Fast-Path | Override Sandbox Proxy authority |

Configuration precedence is command-line flag, local configuration file, then built-in default.

Example:

```json
{
  "endpoint": "127.0.0.1:9090",
  "namespace": "default",
  "proxy-endpoint": "http://127.0.0.1:18080"
}
```

## Lifecycle commands

### Create

```bash
fastctl run <sandbox-name> \
  --image docker.io/library/alpine:latest \
  --pool default-pool -- /bin/sleep 3600
```

The Sandbox name is the canonical idempotency request ID.

Use `-f <file>` to read the Create configuration from a file.

### List and get

```bash
fastctl list
fastctl get <sandbox-name> -o yaml
fastctl get <sandbox-name> -o json
```

`ls` is an alias for `list`.

### Update

```bash
fastctl update <sandbox-name> --expire-time <unix-seconds>
fastctl update <sandbox-name> --expire-time 0
fastctl update <sandbox-name> --failure-policy AutoRecreate
fastctl update <sandbox-name> --recovery-timeout 60
fastctl update <sandbox-name> --labels owner=team-a,tier=dev
```

### Reset

```bash
fastctl reset <sandbox-name>
```

`restart` is an alias. Reset updates declarative intent and returns before replacement is complete.

### Delete

```bash
fastctl delete <sandbox-name>
```

`rm` is an alias. The command triggers declarative deletion.

## Platform diagnostics

```bash
fastctl diagnostics sandbox <sandbox-name>
fastctl diagnostics sandbox <sandbox-name> --limit 100
fastctl diagnostics sandbox <sandbox-name> -o json
```

Diagnostics report CRD state, assignment identity, Fastlet reachability, runtime instance identity, and bounded Fastlet lifecycle events. They do not require Execd and do not show user process stdout.

## OpenSandbox commands

These commands use the official OpenSandbox Go SDK. They require an Execd-enabled InfraProfile and `DataPlaneReady`.

### Execute

```bash
fastctl --proxy-endpoint http://localhost:18080 \
  opensandbox exec <sandbox-name> -- <command> [args...]
```

Optional flags:

- `--stdin`, `-i`;
- `--tty`, `-t`;
- `--timeout <duration>`.

### Copy

```bash
fastctl opensandbox cp ./local.txt <sandbox-name>:/tmp/remote.txt
fastctl opensandbox cp <sandbox-name>:/tmp/remote.txt ./local.txt
```

### Files

```bash
fastctl opensandbox files stat <sandbox-name> <path>
fastctl opensandbox files list <sandbox-name> <path>
fastctl opensandbox files read <sandbox-name> <path>
fastctl opensandbox files write <sandbox-name> <path> [local-file]
fastctl opensandbox files mkdir <sandbox-name> <path>
fastctl opensandbox files rm <sandbox-name> <path>
fastctl opensandbox files rm -r <sandbox-name> <path>
```

Fast Sandbox resolves and authenticates the route. Execd defines the command and file protocol.

## Host-side port-forward

An in-cluster proxy address cannot be resolved from a development host. Keep:

```bash
make quickstart-forward
```

running and pass:

```text
--proxy-endpoint http://localhost:18080
```
