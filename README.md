# nico-core-mock

Mock NICo Forge gRPC server with machines defined in YAML. Includes a Helm chart for local Kubernetes deployment.

## Local kind workflow

Build and deploy assuming:

- `nico-core-mock` and `infra-controller` are sibling directories under the same parent (required for the Docker build context)
- A kind cluster named `nico-rest-local`
- A local registry listening on `localhost:5000`

Run the Docker build from the **parent** of `nico-core-mock` (the directory that contains both `nico-core-mock/` and `infra-controller/`):

```bash
docker build -f nico-core-mock/Dockerfile -t localhost:5000/nico-core-mock:latest .

kind load docker-image localhost:5000/nico-core-mock:latest --name nico-rest-local
```

From the **nico-core-mock** repository root, deploy with Helm:

```bash
kubectl create namespace nico-rest --dry-run=client -o yaml | kubectl apply -f -

helm upgrade --install nico-rest-mock-core helm/nico-rest-mock-core/ \
  --namespace nico-rest \
  --set image.repository=localhost:5000/nico-core-mock \
  --set image.tag=latest \
  --set image.pullPolicy=Never \
  --set libvirt.enabled=true \
  --set libvirt.endpoint="qemu+tcp://172.19.120.248/system"
```

The gRPC server listens on port `11079`. Port-forward to reach it from the host:

```bash
kubectl port-forward -n nico-rest svc/nico-rest-mock-core 11079:11079
```

Mock hosts (machines + discovery metadata) are defined statically in
`helm/nico-rest-mock-core/values.yaml` under the `inventory` key. The chart
copies that into a ConfigMap at deploy time. Lab installs may override
`inventory` with a Helm `-f` values file (for example from ufo-simulator).

To run locally without Kubernetes (same file the chart uses):

```bash
go run ./cmd/nico-core-mock --config helm/nico-rest-mock-core/values.yaml
```

When libvirt filtering is enabled, only inventory machines whose `id` matches an existing libvirt domain (by domain name or UUID) are returned by `FindMachineIds` and `FindMachinesByIds`:

```bash
go run ./cmd/nico-core-mock \
  --config helm/nico-rest-mock-core/values.yaml \
  --libvirt-endpoint=qemu+tcp://192.168.122.1:16509/system
```

Helm values:

```yaml
libvirt:
  enabled: true
  endpoint: qemu+tcp://192.168.122.1:16509/system
```

When libvirt provisioning runs with instance user-data, it is injected into the root disk with `virt-customize` automatically. No extra helm values are required for injection (the chart always runs privileged as root and sets `LIBGUESTFS_BACKEND=direct`).
