# Validate Resource-Aware Capacity and Pool Elasticity

This guide validates the `ResourceAware` resource contract on a development
Kubernetes cluster. It covers the two elasticity paths that matter for
capacity operations:

1. normal Sandbox demand scales a Pool out, and released demand scales it down
   after the stabilization window;
2. a memory-blocked Fastlet reports zero effective free capacity through its
   heartbeat, causing the Pool to add replacement capacity and reject new
   placement on blocked Fastlets.

The examples use the `container` runtime and the repository's basic kind
profile. Run the commands from the repository root on a Linux development
host with Docker, kind, kubectl, and Go installed.

## Prepare the cluster

Build the current checkout, create or reuse the basic kind cluster, load the
images, and deploy the control plane:

```bash
make env PROFILE=basic
kubectl config use-context kind-fsb-e2e-basic
kubectl get nodes -o wide
kubectl get pods -n fast-sandbox-system
```

Create an isolated namespace:

```bash
kubectl create namespace resource-aware-manual
```

## Scenario A: scale on Sandbox demand

### 1. Create the Pool

Apply a Pool with two Sandbox commitments per Fastlet. Do not set CPU or
memory resources on the first Fastlet container; the Controller owns those
values in `ResourceAware` mode.

```bash
kubectl apply -f - <<'EOF'
apiVersion: sandbox.fast.io/v1alpha2
kind: SandboxPool
metadata:
  name: resource-aware-pool
  namespace: resource-aware-manual
spec:
  runtime: container
  capacity:
    poolMin: 1
    poolMax: 2
    bufferMin: 0
    bufferMax: 0
  maxSandboxesPerPod: 2
  sandboxResources:
    requests:
      cpu: 100m
      memory: 128Mi
    limits:
      cpu: 500m
      memory: 192Mi
    pids: 64
  fastletTemplate:
    spec:
      containers:
      - name: fastlet
        image: fast-sandbox/fastlet:dev
        imagePullPolicy: IfNotPresent
        env:
        - name: FAST_SANDBOX_MEMORY_HIGH_WATERMARK
          value: "0.99"
        - name: FAST_SANDBOX_MEMORY_LOW_WATERMARK
          value: "0.95"
        - name: FAST_SANDBOX_MEMORY_PSI_AVG10_THRESHOLD
          value: "0"
        - name: FAST_SANDBOX_NODE_MEMORY_LOW_WATERMARK
          value: "0"
        - name: FAST_SANDBOX_NODE_MEMORY_RECOVERY_WATERMARK
          value: "0"
EOF
```

The expected resource contract is:

```text
commitmentCapacity.cpu    = 100m * 2 = 200m
commitmentCapacity.memory = 128Mi * 2 = 256Mi

runtimeEnvelope.cpu
  = 100m container-runtime overhead + max(200m, 500m)
  = 600m

runtimeEnvelope.memory
  = 128Mi container-runtime overhead + max(256Mi, 192Mi)
  = 384Mi
```

The watermark overrides above make this static-capacity scenario insensitive
to temporary pressure on a small development node. They are not recommended
production defaults.

### 2. Wait for the initial Fastlet

```bash
kubectl wait -n resource-aware-manual \
  --for=condition=Ready pod \
  -l fast-sandbox.io/pool=resource-aware-pool \
  --timeout=120s

kubectl get sandboxpool resource-aware-pool \
  -n resource-aware-manual -o wide

kubectl get pods -n resource-aware-manual \
  -l fast-sandbox.io/pool=resource-aware-pool -o wide
```

Exactly one Fastlet Pod should be ready.

### 3. Verify the derived contract

Read the generated Pod resources and commitment annotation:

```bash
FASTLET_POD="$(kubectl get pods -n resource-aware-manual \
  -l fast-sandbox.io/pool=resource-aware-pool \
  -o jsonpath='{.items[0].metadata.name}')"

kubectl get pod "$FASTLET_POD" -n resource-aware-manual \
  -o jsonpath='{.spec.containers[0].resources}{"\n"}{.metadata.annotations.fast-sandbox\.io/commitment-capacity}{"\n"}'
```

The first container must have equal Kubernetes requests and limits of `600m`
CPU and `384Mi` memory. The annotation must contain `200m` CPU and `256Mi`
memory. The envelope includes fixed Fastlet overhead; the commitment
annotation does not.

Inspect the immutable Fastlet settings delivered by the Controller:

```bash
SETTINGS_CM="$(kubectl get pod "$FASTLET_POD" -n resource-aware-manual \
  -o jsonpath='{.spec.volumes[?(@.name=="fastlet-settings")].configMap.name}')"

kubectl get configmap "$SETTINGS_CM" -n resource-aware-manual \
  -o jsonpath='{.data.resource-admission\.json}' | jq .
```

The document should report `capacity` as `200m` and `256Mi`, and
`runtimeLimit` as `600m` and `384Mi`.

### 4. Fill the first Fastlet

Create two long-running Sandboxes:

```bash
kubectl apply -f - <<'EOF'
apiVersion: sandbox.fast.io/v1alpha2
kind: Sandbox
metadata:
  name: ra-sandbox-1
  namespace: resource-aware-manual
spec:
  image: docker.io/library/alpine:latest
  command: ["/bin/sleep", "3600"]
  poolRef: resource-aware-pool
---
apiVersion: sandbox.fast.io/v1alpha2
kind: Sandbox
metadata:
  name: ra-sandbox-2
  namespace: resource-aware-manual
spec:
  image: docker.io/library/alpine:latest
  command: ["/bin/sleep", "3600"]
  poolRef: resource-aware-pool
EOF

kubectl wait -n resource-aware-manual \
  --for=jsonpath='{.status.runtime.state}'=Ready \
  sandbox/ra-sandbox-1 sandbox/ra-sandbox-2 \
  --timeout=120s
```

Both Sandboxes should use the same Fastlet, and the Pool should still have one
Fastlet Pod:

```bash
kubectl get sandboxes -n resource-aware-manual \
  -o custom-columns='NAME:.metadata.name,STATE:.status.runtime.state,FASTLET:.status.placement.fastletName'

kubectl get pods -n resource-aware-manual \
  -l fast-sandbox.io/pool=resource-aware-pool \
  --no-headers | wc -l
```

### 5. Trigger scale-out

Create a third Sandbox. The first Fastlet has exhausted both of its static
commitments, so the Sandbox remains pending until the Pool creates a second
Fastlet.

```bash
kubectl apply -f - <<'EOF'
apiVersion: sandbox.fast.io/v1alpha2
kind: Sandbox
metadata:
  name: ra-sandbox-3
  namespace: resource-aware-manual
spec:
  image: docker.io/library/alpine:latest
  command: ["/bin/sleep", "3600"]
  poolRef: resource-aware-pool
EOF

for attempt in $(seq 1 60); do
  FASTLET_COUNT="$(kubectl get pods -n resource-aware-manual \
    -l fast-sandbox.io/pool=resource-aware-pool \
    --no-headers | wc -l | tr -d ' ')"
  [ "$FASTLET_COUNT" = "2" ] && break
  sleep 2
done
test "$FASTLET_COUNT" = "2"

kubectl wait -n resource-aware-manual \
  --for=jsonpath='{.status.runtime.state}'=Ready \
  sandbox/ra-sandbox-3 \
  --timeout=120s

kubectl get sandboxes -n resource-aware-manual \
  -o custom-columns='NAME:.metadata.name,STATE:.status.runtime.state,FASTLET:.status.placement.fastletName'
```

There must be two distinct Fastlet names, and no Fastlet may own more than two
Sandboxes.

### 6. Trigger stabilized scale-down

Delete all demand:

```bash
kubectl delete sandbox -n resource-aware-manual \
  ra-sandbox-1 ra-sandbox-2 ra-sandbox-3
```

The Pool intentionally keeps two Fastlets during the default one-minute
stabilization window. Inspect the persisted candidate timestamp:

```bash
kubectl get sandboxpool resource-aware-pool -n resource-aware-manual \
  -o jsonpath='{.status.resourceScaleDownCandidateSince}{"\n"}'
```

Wait for the excess idle Fastlet to drain and disappear:

```bash
for attempt in $(seq 1 90); do
  FASTLET_COUNT="$(kubectl get pods -n resource-aware-manual \
    -l fast-sandbox.io/pool=resource-aware-pool \
    --no-headers | wc -l | tr -d ' ')"
  [ "$FASTLET_COUNT" = "1" ] && break
  sleep 2
done
test "$FASTLET_COUNT" = "1"
```

The final count must equal `poolMin: 1`.

## Scenario B: force the memory-pressure path

This scenario is test-only fault injection. It sets a memory high watermark
below normal Fastlet baseline usage so the real cgroup observer produces a
`BLOCKED` heartbeat without consuming large amounts of memory. Never copy
these watermark values into a production Pool.

### 1. Create a pressure-injection Pool

```bash
kubectl apply -f - <<'EOF'
apiVersion: sandbox.fast.io/v1alpha2
kind: SandboxPool
metadata:
  name: resource-pressure-pool
  namespace: resource-aware-manual
spec:
  runtime: container
  capacity:
    poolMin: 1
    poolMax: 2
    bufferMin: 1
    bufferMax: 1
  maxSandboxesPerPod: 2
  sandboxResources:
    requests:
      cpu: 100m
      memory: 128Mi
    limits:
      cpu: 500m
      memory: 192Mi
    pids: 64
  fastletTemplate:
    spec:
      containers:
      - name: fastlet
        image: fast-sandbox/fastlet:dev
        imagePullPolicy: IfNotPresent
        env:
        - name: FAST_SANDBOX_MEMORY_OBSERVER_INTERVAL
          value: "250ms"
        - name: FAST_SANDBOX_MEMORY_HIGH_WATERMARK
          value: "0.01"
        - name: FAST_SANDBOX_MEMORY_LOW_WATERMARK
          value: "0.005"
        - name: FAST_SANDBOX_MEMORY_HIGH_SAMPLES
          value: "1"
        - name: FAST_SANDBOX_MEMORY_RECOVERY_SAMPLES
          value: "1"
        - name: FAST_SANDBOX_MEMORY_PSI_AVG10_THRESHOLD
          value: "0"
        - name: FAST_SANDBOX_NODE_MEMORY_LOW_WATERMARK
          value: "0"
        - name: FAST_SANDBOX_NODE_MEMORY_RECOVERY_WATERMARK
          value: "0"
EOF
```

Watch the Pool grow from one Fastlet to `poolMax: 2`:

```bash
kubectl get pods -n resource-aware-manual \
  -l fast-sandbox.io/pool=resource-pressure-pool \
  --watch
```

The first Fastlet's blocked heartbeat contributes zero effective free
capacity. Because `bufferMin` requires one usable commitment, the Controller
adds another Fastlet. The second Fastlet is deliberately blocked by the same
test watermark, so the Pool stops at `poolMax`.

### 2. Inspect the heartbeat state

In one terminal, forward a Fastlet control port:

```bash
PRESSURE_POD="$(kubectl get pods -n resource-aware-manual \
  -l fast-sandbox.io/pool=resource-pressure-pool \
  -o jsonpath='{.items[0].metadata.name}')"

kubectl port-forward -n resource-aware-manual \
  pod/"$PRESSURE_POD" 5758:5758
```

In another terminal, read the real heartbeat payload:

```bash
curl -fsS http://127.0.0.1:5758/api/v2/fastlet/heartbeat \
  | jq '.admission.resources'
```

Expected fields include:

```json
{
  "state": "BLOCKED",
  "reason": "FASTLET_MEMORY_HIGH_WATERMARK"
}
```

### 3. Verify that new placement is blocked

```bash
kubectl apply -f - <<'EOF'
apiVersion: sandbox.fast.io/v1alpha2
kind: Sandbox
metadata:
  name: ra-pressure-blocked
  namespace: resource-aware-manual
spec:
  image: docker.io/library/alpine:latest
  command: ["/bin/sleep", "3600"]
  poolRef: resource-pressure-pool
EOF

sleep 10
kubectl get sandbox ra-pressure-blocked -n resource-aware-manual \
  -o custom-columns='NAME:.metadata.name,STATE:.status.runtime.state,FASTLET:.status.placement.fastletName'
```

The Fastlet column must remain empty and the Sandbox must not become Ready.
Dynamic memory admission blocks new creation only; it does not reclaim,
pause, migrate, or evict an already running Sandbox.

## Run the automated equivalent

Run every scheduling scenario, including LegacySlot regression coverage:

```bash
make e2e SUITE=scheduling E2E_TEST_TIMEOUT=30m
```

Run only the normal ResourceAware elasticity scenario:

```bash
go test ./test/e2e/suites/scheduling \
  -run '^TestResourceAwareOvercommitScaling$' \
  -v -count=1 -timeout 30m
```

Run only the pressure and heartbeat scenario:

```bash
go test ./test/e2e/suites/scheduling \
  -run '^TestResourceAwareMemoryPressureScaling$' \
  -v -count=1 -timeout 20m
```

Each test allocates a unique namespace and removes it on success. Set
`FAST_SANDBOX_E2E_PRESERVE_NAMESPACE=1` before the command when a failed
namespace must be retained for inspection.

## Troubleshooting

### The Pool is rejected

```bash
kubectl describe sandboxpool -n resource-aware-manual resource-aware-pool
```

Check the `RuntimeReady` condition. A `ResourceProfileInvalid` reason usually
means requests or limits are incomplete, a limit is below its request, or the
first Fastlet container specifies CPU or memory resources in `ResourceAware`
mode.

### The Pool does not scale out

Check all four constraints:

- the Pool has not reached `poolMax`;
- pending demand or `bufferMin` exceeds effective free capacity;
- Fastlet heartbeats are fresh;
- ResourceAware Pool scaling and placement feature gates are enabled.

```bash
kubectl get sandboxpool -n resource-aware-manual -o yaml
kubectl get sandboxes -n resource-aware-manual -o wide
kubectl get pods -n resource-aware-manual \
  -l fast-sandbox.io/pool=resource-aware-pool -o wide
kubectl logs -n fast-sandbox-system \
  -l control-plane=controller-manager \
  --all-containers --since=10m \
  | grep -E 'scaling up fastlet pool|drain|resource'
```

### The Pool does not scale down

Scale-down requires all of the following:

- no pending Sandbox demand;
- an idle Fastlet that can be removed while retaining `bufferMax` and
  `bufferMin`;
- continuous excess capacity for the stabilization window;
- no drain failure on the selected Fastlet.

Read `status.resourceScaleDownCandidateSince`, list Sandbox placements, and
inspect Controller logs before changing the Pool.

## Clean up

Deleting the namespace removes both Pools, their Fastlet Pods, and all manual
Sandbox objects:

```bash
kubectl delete namespace resource-aware-manual
```
