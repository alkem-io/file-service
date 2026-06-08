# K8s Deployment Manifest (Example)

Based on the existing TS file-service manifest at
`/Users/antst/work/alkemio/file-service/manifests/25-file-service-deployment-dev.yaml`.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: alkemio-file-service-deployment
  labels:
    app: alkemio-file-service
spec:
  replicas: 1
  selector:
    matchLabels:
      app: alkemio-file-service
  template:
    metadata:
      labels:
        app: alkemio-file-service
    spec:
      securityContext:
        fsGroup: 65532
        runAsNonRoot: true
      containers:
        - name: alkemio-file-service
          image: rg.nl-ams.scw.cloud/alkemio/alkemio-file-service:latest
          ports:
            - containerPort: 4003
          env:
            - name: PORT
              value: "4003"
            - name: LOCAL_STORAGE_PATH
              value: "/storage"
            - name: STORAGE_TYPE
              value: "local"
            - name: DOCUMENT_MAX_AGE
              value: "86400"
          envFrom:
            - configMapRef:
                name: alkemio-config       # Shared: NATS_URL, NATS_* settings
            - secretRef:
                name: alkemio-secrets      # ALKEMIO_DATABASE_* credentials
          volumeMounts:
            - name: file-storage
              mountPath: /storage
              # NOTE: read-write (not read-only) — file-service handles writes
          livenessProbe:
            httpGet:
              path: /health
              port: 4003
            initialDelaySeconds: 5
            periodSeconds: 10
          readinessProbe:
            httpGet:
              path: /health
              port: 4003
            initialDelaySeconds: 3
            periodSeconds: 5
      volumes:
        - name: file-storage
          persistentVolumeClaim:
            claimName: file-storage-pvc2
```

## Key Differences from TS file-service manifest

| Aspect | TS file-service | Go file-service |
|--------|----------------|-----------------|
| Image | `alkemio-file-service:latest` | `alkemio-file-service:latest` |
| Port | 4003 | 4003 (same) |
| Volume mount | read-only | **read-write** (file-service handles writes now) |
| RabbitMQ env | `RABBITMQ_*` secrets | Not needed (uses NATS from `alkemio-config`) |
| NATS env | Not present | From `alkemio-config` ConfigMap |
| DB env | Not present | `ALKEMIO_DATABASE_*` from `alkemio-secrets` |
| Health probe | `/health/` | `/health` (no trailing slash) |
| Runtime | Node.js distroless | Go distroless + libvips |
