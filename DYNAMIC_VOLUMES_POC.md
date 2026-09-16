# Dynamic Volumes PoC (`ConfigMap` & Disk-Backed `emptyDir` Hot-Plug & Hot-Unplug)

This guide documents how to build, deploy, and test live **Hot-Plug** and **Hot-Unplug** of `ConfigMap` and disk-backed `emptyDir` volumes into running containers on a `kind` cluster without restarting containers.

---

## Architectural Summary of PoC Changes

1. **API Validation Relaxation (`pkg/apis/core/validation/validation.go`)**:
   - Relaxes Pod immutability in `ValidatePodUpdate` so `spec.volumes` and `spec.containers[*].volumeMounts` can be added and removed via `kubectl patch pod` or `kubectl edit pod`.
2. **Kubelet `DesiredStateOfWorldPopulator` (`pkg/kubelet/volumemanager/populator/desired_state_of_world_populator.go`)**:
   - **Hot-Plug Addition (`processPodVolumes`)**: Allows already-processed running pods to discover and mount newly added volumes in `pod.Spec.Volumes`.
   - **Hot-Unplug Teardown (`findAndRemoveDeletedPods`)**: Bypasses the container termination barrier (`ShouldPodRuntimeBeRemoved`) when a volume has been removed from `pod.Spec.Volumes`, allowing `DeletePodFromVolume` and `UnmountVolume` to execute while the container continues running.
3. **Host-to-Container Mount Propagation Bridge (`pkg/volume/util/dynamic_mounts.go`, `pkg/volume/configmap/configmap.go`, `pkg/volume/emptydir/empty_dir.go`)**:
   - Ensures any `emptyDir` volume mounted with `mountPropagation: HostToContainer` (or `Bidirectional`) is marked `MS_SHARED` (`rshared`) on the host.
   - When a `ConfigMap` or `emptyDir` volume is mounted (`SetUpAt`) at a container path that is a subdirectory of a `HostToContainer` parent volume mount (e.g. `/mnt/dynamic/cm1` or `/mnt/dynamic/scratch` inside `/mnt/dynamic`), `volumeutil.SyncDynamicPropagationMounts` bind-mounts the volume directory into the corresponding host parent directory (`/var/lib/kubelet/pods/<uid>/volumes/kubernetes.io~empty-dir/dynamic-root/<subpath>`). The Linux kernel VFS immediately propagates the submount into the running container.
   - When the volume is removed from the Pod spec, `volumeutil.CleanupDynamicPropagationMounts` in `TearDownAt` unmounts and deletes the propagated host submount, immediately unmounting it inside the running container.

---

## Step 1: Build Modified Binaries

Compile `kube-apiserver` and `kubelet`:

```bash
make WHAT="cmd/kube-apiserver cmd/kubelet"
```

---

## Step 2: Inject Binaries into Running `kind` Cluster

Assuming your `kind` node container is named `kind-control-plane` (adjust if using a custom cluster name, e.g., `kind get nodes`):

```bash
NODE_NAME="kind-control-plane"

# 1. Replace kubelet binary and restart kubelet service
docker cp _output/bin/kubelet "${NODE_NAME}:/usr/bin/kubelet"
docker exec "${NODE_NAME}" systemctl restart kubelet

# 2. Replace kube-apiserver binary inside the static pod
# Copy binary to host path mounted by kube-apiserver or directly into container image
docker cp _output/bin/kube-apiserver "${NODE_NAME}:/usr/local/bin/kube-apiserver-custom"
docker exec "${NODE_NAME}" bash -c '
  APISERVER_CONTAINER=$(crictl ps --name kube-apiserver -q | head -n1)
  # Move static manifest temporarily to restart kube-apiserver with updated binary mounted
  sed -i "s|command:|volumeMounts:\n    - mountPath: /usr/local/bin/kube-apiserver\n      name: custom-apiserver\n      readOnly: true\n  command:|" /etc/kubernetes/manifests/kube-apiserver.yaml
  sed -i "s|volumes:|volumes:\n  - hostPath:\n      path: /usr/local/bin/kube-apiserver-custom\n      type: File\n    name: custom-apiserver|" /etc/kubernetes/manifests/kube-apiserver.yaml
'
```

Wait ~15 seconds for `kube-apiserver` and `kubelet` to report Ready:

```bash
kubectl get nodes
```

---

## Step 3: Create Test ConfigMap & Running Pod with Mount Propagation

1. Create a `ConfigMap` that we will dynamically hot-plug later:

```bash
kubectl create configmap dynamic-cm \
  --from-literal=greeting="Hello from dynamically hot-plugged ConfigMap!" \
  --from-literal=environment="production"
```

2. Create a Pod with a parent `emptyDir` volume mounted at `/mnt/dynamic` using `mountPropagation: HostToContainer`:

```bash
cat << 'EOF' | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: dynamic-vol-demo
spec:
  automountServiceAccountToken: false
  containers:
  - name: app
    image: busybox:1.36
    command: ["sh", "-c", "exec sleep 36000"]
    volumeMounts:
    - name: dynamic-root
      mountPath: /mnt/dynamic
      mountPropagation: HostToContainer
  volumes:
  - name: dynamic-root
    emptyDir: {}
EOF
```

3. Verify the Pod is running and `/mnt/dynamic` is initially empty:

```bash
kubectl wait --for=condition=Ready pod/dynamic-vol-demo --timeout=30s
kubectl exec dynamic-vol-demo -- ls -la /mnt/dynamic
```

---

## Step 4: Test Live Hot-Plug (Dynamic Attach)

Patch the running Pod to add `dynamic-cm` to `spec.volumes` and mount it at `/mnt/dynamic/cm1`:

```bash
kubectl patch pod dynamic-vol-demo --type='json' -p='[
  {
    "op": "add",
    "path": "/spec/volumes/-",
    "value": {
      "name": "dyn-cm",
      "configMap": {
        "name": "dynamic-cm"
      }
    }
  },
  {
    "op": "add",
    "path": "/spec/containers/0/volumeMounts/-",
    "value": {
      "name": "dyn-cm",
      "mountPath": "/mnt/dynamic/cm1"
    }
  }
]'
```

Verify that:
1. The container did **not** restart (`RESTARTS` count is `0`).
2. The `ConfigMap` files are immediately readable inside the running container at `/mnt/dynamic/cm1`:

```bash
kubectl get pod dynamic-vol-demo
# NAME               READY   STATUS    RESTARTS   AGE
# dynamic-vol-demo   1/1     Running   0          2m

kubectl exec dynamic-vol-demo -- ls -la /mnt/dynamic/cm1
kubectl exec dynamic-vol-demo -- cat /mnt/dynamic/cm1/greeting
# Output: Hello from dynamically hot-plugged ConfigMap!
```

---

## Step 5: Test Live Hot-Unplug (Dynamic Detach)

Patch the running Pod to remove `dyn-cm` from `spec.volumes` and `spec.containers[0].volumeMounts`:

```bash
kubectl patch pod dynamic-vol-demo --type='json' -p='[
  {
    "op": "remove",
    "path": "/spec/containers/0/volumeMounts/1"
  },
  {
    "op": "remove",
    "path": "/spec/volumes/1"
  }
]'
```

Verify that:
1. The container is still running with **zero restarts** (`RESTARTS = 0`).
2. `/mnt/dynamic/cm1` has been unmounted and removed from inside the running container:

```bash
kubectl get pod dynamic-vol-demo
# NAME               READY   STATUS    RESTARTS   AGE
# dynamic-vol-demo   1/1     Running   0          4m

kubectl exec dynamic-vol-demo -- ls -la /mnt/dynamic
# Output shows /mnt/dynamic is empty again!
```

---

## Step 6: Test Live Hot-Plug of Disk-Backed `emptyDir` (Dynamic Attach)

Patch the running Pod to dynamically add a disk-backed `emptyDir` volume (`dyn-scratch`) to `spec.volumes` and mount it at `/mnt/dynamic/scratch`:

```bash
kubectl patch pod dynamic-vol-demo --type='json' -p='[
  {
    "op": "add",
    "path": "/spec/volumes/-",
    "value": {
      "name": "dyn-scratch",
      "emptyDir": {}
    }
  },
  {
    "op": "add",
    "path": "/spec/containers/0/volumeMounts/-",
    "value": {
      "name": "dyn-scratch",
      "mountPath": "/mnt/dynamic/scratch"
    }
  }
]'
```

Verify that:
1. The container did **not** restart (`RESTARTS = 0`).
2. The new disk-backed directory `/mnt/dynamic/scratch` is immediately mounted and writable inside the running container:

```bash
kubectl get pod dynamic-vol-demo
# NAME               READY   STATUS    RESTARTS   AGE
# dynamic-vol-demo   1/1     Running   0          6m

# Write data to the dynamically attached emptyDir
kubectl exec dynamic-vol-demo -- sh -c 'echo "Hello from dynamically hot-plugged disk-backed emptyDir!" > /mnt/dynamic/scratch/data.txt'

# Read it back
kubectl exec dynamic-vol-demo -- ls -la /mnt/dynamic/scratch
kubectl exec dynamic-vol-demo -- cat /mnt/dynamic/scratch/data.txt
# Output: Hello from dynamically hot-plugged disk-backed emptyDir!
```

---

## Step 7: Test Live Hot-Unplug of Disk-Backed `emptyDir` (Dynamic Detach)

Patch the running Pod to remove `dyn-scratch` from `spec.volumes` and `spec.containers[0].volumeMounts`:

```bash
kubectl patch pod dynamic-vol-demo --type='json' -p='[
  {
    "op": "remove",
    "path": "/spec/containers/0/volumeMounts/1"
  },
  {
    "op": "remove",
    "path": "/spec/volumes/1"
  }
]'
```

Verify that:
1. The container is still running with **zero restarts** (`RESTARTS = 0`).
2. `/mnt/dynamic/scratch` has been cleanly unmounted and removed from inside the running container:

```bash
kubectl get pod dynamic-vol-demo
# NAME               READY   STATUS    RESTARTS   AGE
# dynamic-vol-demo   1/1     Running   0          8m

kubectl exec dynamic-vol-demo -- ls -la /mnt/dynamic
# Output shows /mnt/dynamic is empty again!
```
