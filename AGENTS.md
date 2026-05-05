# AI Development Instructions

## Remote Runtime Policy

This repository is often edited locally on macOS, but Linux container runtime behavior must be validated on the remote development VM.

Use `$remote-dev-run` for any task that needs complex runtime infrastructure, including:

- e2e tests
- Kubernetes or kind clusters
- kata, gVisor, secure containers, or RuntimeClass behavior
- containerd, CRI, Docker daemon, image loading, or runtime shims
- commands that create pods, BatchSandboxes, SandboxSnapshots, clusters, images, or secure-runtime workloads
- debugging controller behavior that depends on a live Kubernetes cluster or container runtime

Do not try to validate those flows locally on macOS. Local execution is acceptable for:

- editing files
- formatting, such as `gofmt`
- static inspection
- unit tests that do not require Linux-only container runtime behavior
- small package tests that do not create Kubernetes/container runtime resources

## Remote Environment

The default remote development environment for this repository is:

```text
SSH host: ssh-fast
Remote repo: ~/fast-sandbox
```

When remote execution is needed:

1. Inspect local changes with `git status --short --branch`.
2. Use `$remote-dev-run` to sync the needed local changes to the remote VM.
3. Run the test or debug command remotely from `~/fast-sandbox`.
4. Report the exact remote command, exit status, and key output lines.

If `.remote-dev-run.env` is used for project-specific settings, it must remain local and git-ignored. If it is missing or not ignored, ask the user before creating config or changing ignore rules.

## Testing Judgment

Prefer remote validation when in doubt. In particular, any command involving kata/gVisor setup, runtime classes, kind cluster creation, controller deployment, or Kubernetes e2e behavior should run through `$remote-dev-run`, not directly on the local machine.
