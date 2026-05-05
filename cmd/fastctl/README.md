# fastctl (Fast Sandbox Control)

`fastctl` is the official command-line interface for Fast Sandbox. It provides a developer-friendly way to manage sandboxes with millisecond-level latency, bypassing the traditional Kubernetes control plane overhead for rapid iterations.

## 🚀 Features

*   **Fast-Path Execution**: Create sandboxes in <50ms using the gRPC Fast-Path API.
*   **Interactive Mode**: Guided configuration for complex sandbox setups without memorizing flags.
*   **Configuration Management**: Hierarchical config loading (Flags > Local File > Global File).
*   **Production Ready**: Built-in support for consistency modes (Fast/Strong) and resource management.

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
*   `--mode`: Consistency mode (`fast` for speed, `strong` for consistency).
*   `--ports`: Exposed ports (e.g., `--ports=8080,9090`).

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

### Consistency Modes
*   **Fast (Default)**: Fastlet creates container first, then asynchronously updates K8s. Lowest latency (<50ms). Best for ephemeral tasks.
*   **Strong**: Writes to K8s ETCD first, then creates container. Guarantees consistency but higher latency (~200ms).

### Interactive Template
When running interactively, you can define advanced fields like environment variables:

```yaml
# fastctl sandbox configuration
image: python:3.9
pool_ref: gpu-pool
consistency_mode: fast
command: ["python", "app.py"]
envs:
  API_KEY: secret-value
  DEBUG: "true"
```
