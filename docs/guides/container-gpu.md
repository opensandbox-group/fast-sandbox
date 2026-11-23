# NVIDIA GPU Sandboxes with containerd

GPU Pools use `runtime: container` and assign exclusive whole NVIDIA GPUs. Kubernetes allocates devices to the Fastlet Pod. Fastlet leases a fixed number to each Sandbox and injects them through CDI. Set up a separate Pool for each GPU count or model. CPU-only Pools omit `gpus`.

## Prepare the nodes

On Linux GPU nodes, install a compatible NVIDIA driver, [NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/cdi-support.html), and the [NVIDIA Kubernetes device plugin](https://github.com/NVIDIA/k8s-device-plugin). Use `DEVICE_ID_STRATEGY=uuid` and `MIG_STRATEGY=none`, with time slicing and MPS disabled. For the two-slot example, one node needs two free GPUs.

On each GPU node, compare `nvidia-smi -L` with `sudo nvidia-ctk cdi list`. Each GPU UUID needs a matching `nvidia.com/gpu=GPU-…` entry. Fastlet reads `/etc/cdi` and `/var/run/cdi`. If the node already generates CDI specs, configure that generator to provide UUID names. On a node without NVIDIA CDI specs, generate one:

```bash
sudo mkdir -p /etc/cdi
sudo nvidia-ctk cdi generate --device-name-strategy=uuid --output=/etc/cdi/nvidia.yaml
sudo nvidia-ctk cdi list
```

Verify that the Fastlet and host containerd can resolve the device, library, and hook paths in the CDI spec. If the node uses a non-default kubelet root or containerd socket, set it in the [runtime environment](runtime-environments.md). The PodResources socket must exist at `<kubelet-root>/pod-resources/kubelet.sock`.

## Deploy a GPU Pool

Install the CRDs with `kubectl apply -k config/crd`. Deploy the Controller and Fast-Path from the same build using the [deployment guide](deployment.md). Copy [the Execd Pool sample](../../config/samples/pool-container-execd.yaml) to `gpu-pool.yaml`, set `metadata.name` to `gpu-pool` and `fastletTemplate.spec.containers[0].image` to the matching Fastlet image. Keep the one-Pod capacity and Execd component. Set these fields in `spec`:

```yaml
runtime: container
maxSandboxesPerPod: 2
sandboxResources:
  cpu: "1"
  memory: 4Gi
  pids: 512
  gpus: 1
```

The Controller requests and limits `nvidia.com/gpu: 2` on Fastlet. Leave `nvidia.com/gpu` out of the container template. For model selection, use homogeneous GPU nodes and set `sandboxResources.gpuRequirements.model` to the exact `nvidia.com/gpu.product` label value. Fastlet checks each assigned device against the configured model and `minimumMemory` (for example, `30Gi`). Either constraint requires working `nvidia-smi` in Fastlet. Create a new Pool to change its resource profile.

```bash
kubectl apply -f gpu-pool.yaml
kubectl -n fast-sandbox get pods -l fast-sandbox.io/pool=gpu-pool -w
```

Wait for the Fastlet Pod to become Ready. If it stays Pending, inspect Pod events for GPU capacity or node selector failures. If Fastlet fails to start, check its logs for PodResources, CDI, or GPU requirement errors.

## Run CUDA and verify reuse

Build `fastctl` with `make build COMPONENT=fastctl`. Point it at your Fast-Path and Sandbox Proxy endpoints. To access the default Services locally, run these in separate terminals:

```bash
kubectl -n fast-sandbox-system port-forward svc/fast-sandbox-fastpath 9090:9090
kubectl -n fast-sandbox-system port-forward svc/fast-sandbox-proxy 18080:8080
```

Use a CUDA-enabled PyTorch image that supports your GPU and host driver. Set `image` to its full reference:

```bash
export FAST_SANDBOX_ENDPOINT=localhost:9090
export FAST_SANDBOX_PROXY_ENDPOINT=http://localhost:18080
image=docker.io/pytorch/pytorch:2.7.1-cuda12.8-cudnn9-runtime

create_gpu_sandbox() {
  bin/fastctl -n fast-sandbox run "$1" --pool gpu-pool --image "$image" -- /bin/sleep 3600
}
probe_gpu() {
  bin/fastctl -n fast-sandbox opensandbox exec "$1" -- nvidia-smi --query-gpu=uuid --format=csv,noheader
  bin/fastctl -n fast-sandbox opensandbox exec "$1" -- python -c \
    'import torch; assert torch.cuda.device_count() == 1; x = torch.ones((32, 32), device="cuda"); assert (x @ x).sum().item() == 32768; print("CUDA OK")'
}

for sandbox in gpu-a gpu-b; do
  create_gpu_sandbox "$sandbox"
  probe_gpu "$sandbox"
done
```

Expect one distinct UUID and `CUDA OK` from each Sandbox. Try `create_gpu_sandbox gpu-c` while both slots are occupied. It should fail for lack of capacity or remain unready. Delete `gpu-a` with `bin/fastctl -n fast-sandbox delete gpu-a`, wait for its CR to disappear, then retry `create_gpu_sandbox gpu-c` and run `probe_gpu gpu-c`. It should report the UUID previously held by `gpu-a`.

To check automatic reclamation, delete `gpu-c` and wait for its CR to disappear. Leave `gpu-b` running. Run the following and check that `gpu-exit` reaches `Failed`:

```bash
bin/fastctl -n fast-sandbox run gpu-exit --pool gpu-pool --image "$image" -- /bin/sleep 60
kubectl -n fast-sandbox get sandbox gpu-exit -w -o 'custom-columns=NAME:.metadata.name,STATE:.status.runtime.state'
```

Run `create_gpu_sandbox gpu-c` and `probe_gpu gpu-c` again while the failed CR still exists. To check restart recovery, restart the Fastlet container in the same Pod and verify the UUID of a surviving Sandbox. For a CPU-only check, use the same PyTorch image in a Pool without `gpus` and verify `torch.cuda.device_count() == 0`. When done:

```bash
kubectl -n fast-sandbox delete sandbox gpu-b gpu-c gpu-exit --ignore-not-found
kubectl delete -f gpu-pool.yaml
```

Fastlet records leases in `/var/lib/fast-sandbox/gpu`. Keep this directory across restarts. Fastlet releases a lease after confirming the GPU task and Sandbox processes are gone. If a replacement cannot schedule for lack of GPUs, the Controller can drain an old Fastlet whose node matches the replacement’s placement and has room after the drain. Drain and delete GPU Pools before rolling back to a build without GPU support.

## Run the standalone driver test

On a Linux GPU host with root access, containerd/runc, and the CDI setup above, pre-pull the image and select an unused UUID from `nvidia-smi -L`:

```bash
image=docker.io/pytorch/pytorch:2.7.1-cuda12.8-cudnn9-runtime
sudo ctr -n k8s.io images pull "$image"
sudo env FSB_GPU_E2E_DEVICES=GPU-REPLACE_WITH_UNUSED_UUID FSB_GPU_E2E_IMAGE="$image" \
  go test ./internal/runtime/containerd -run '^TestGPUContainerdCUDA$' -v -count=1 -timeout=10m
```

`FSB_GPU_E2E_SOCKET` and `FSB_GPU_E2E_SNAPSHOTTER` override the default socket and `overlayfs`. For a custom socket, pass the same endpoint to `ctr --address`. On native Linux, pass two comma-separated UUIDs to verify per-container visibility, exhaustion, journal recovery, and reuse after task exit. Use the Kubernetes smoke test to verify device plugin allocation and Pool rollout.
