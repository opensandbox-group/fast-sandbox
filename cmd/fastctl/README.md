# fastctl (Fast Sandbox Control)

`fastctl` is the official command-line interface for Fast Sandbox. Create uses the multi-active CRD-first FastPath: one API-server Create followed by one atomic Fastlet Create on the happy path.

## 🚀 Features

*   **Fast-Path Execution**: Low-latency create through the gRPC FastPath API with durable CRD identity.
*   **Interactive Mode**: Guided configuration for complex sandbox setups without memorizing flags.
*   **Configuration Management**: Hierarchical config loading (Flags > Local File > Global File).
*   **Idempotent Create**: Sandbox name is the stable `request_id` and prevents duplicate runtimes across retries.
*   **Platform Diagnostics**: CRD state and bounded Fastlet lifecycle events do not depend on execd.
*   **OpenSandbox Integration**: `opensandbox exec/cp/files` delegates protocol semantics to the official OpenSandbox Go SDK.

## 📦 Installation

### Build from Source
```bash
make build
# Binary will be at bin/fastctl
```

### Pre-built Binary
Download the latest release from the repository releases page and add it to your `$PATH`.

## ⚙️ Configuration

`fastctl` supports a hierarchical configuration system. It looks for a `config.json` file in the following order:
1.  Current directory: `./.fastctl/config.json`
2.  Home directory: `~/.fastctl/config.json`

**Example `config.json`:**
```json
{
  "endpoint": "127.0.0.1:9090",
  "namespace": "default",
  "editor": "vim"
}
```

You can also override these settings using global flags:
```bash
fastctl --endpoint=10.0.0.1:9090 --namespace=dev ...
```

## 📖 Usage Guide

### 1. Create a Sandbox (`run`)

**Method A: Interactive Mode (Recommended)**
Simply provide a name, and `fastctl` will open your default editor with a template.
```bash
fastctl run my-sandbox
```

**Method B: Flag-based (For Scripts)**
```bash
fastctl run my-sandbox --image=alpine:latest --pool=default-pool --command="/bin/sleep 3600"
```

**Method C: Config File**
```bash
fastctl run my-sandbox -f sandbox-config.yaml
```

**Key Flags:**
*   `--image`: Container image (required in non-interactive mode).
*   `--pool`: Target SandboxPool name (default: `default-pool`).
*   `--request-id`: Compatibility flag; when present it must equal the Sandbox name.

### 2. List Sandboxes (`list`)

View all active sandboxes, including those pending CRD synchronization.
```bash
fastctl list
# OR
fastctl ls
```

### 3. Get Details (`get`)

Inspect metadata for a specific sandbox.
```bash
fastctl get my-sandbox
# JSON output
fastctl get my-sandbox -o json
```

### 4. Delete a Sandbox (`delete`)

Terminate a sandbox immediately.
```bash
fastctl delete my-sandbox
# OR
fastctl rm my-sandbox
```

## 🛠 Advanced Topics

### OpenSandbox Execd

```bash
fastctl opensandbox exec my-sandbox -- /bin/sh -lc 'echo hello'
fastctl opensandbox cp ./input.txt my-sandbox:/tmp/input.txt
fastctl opensandbox files stat my-sandbox /tmp/input.txt
```

These commands resolve and authenticate the route, then use the official
OpenSandbox Go SDK. Fast Sandbox does not duplicate the Execd wire protocol.

### Platform diagnostics

```bash
fastctl diagnostics sandbox my-sandbox
fastctl diagnostics sandbox my-sandbox -o json --limit 100
```

This reports platform lifecycle events, not stdout/stderr of a process in the Sandbox.

### Create Semantics

Create is CRD-first. The initial assignment annotation is committed in the CRD Create, followed by one atomic Fastlet Create. Only an explicit rejection before side effects may CAS the annotation to another Top-K candidate. Ambiguous outcomes stay on the same runtime identity for Controller takeover. Delete, reset, expiry, and failure-policy changes are declarative CRD updates reconciled by the Controller.

### Interactive Template
When running interactively, you can define advanced fields like environment variables:

```yaml
# fastctl sandbox configuration
image: python:3.9
pool_ref: gpu-pool
command: ["python", "app.py"]
envs:
  API_KEY: secret-value
  DEBUG: "true"
```

SandboxPool manifests must use the canonical `runtime`, `sandboxResources`, and `maxSandboxesPerPod` fields.
