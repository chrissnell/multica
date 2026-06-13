# In-cluster Buildkit

## Why a Deployment, not rootless DaemonSet

We run a single multi-tenant Buildkit Deployment, fronted by a Service the
runner pods address via `tcp://buildkitd.multica-ci.svc:1234`. One daemon
means one shared cache PVC, which is exactly the win we want over
GitHub-hosted runners.

## Install

```bash
kubectl create namespace multica-ci

cat <<'EOF' | kubectl apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: buildkit-cache
  namespace: multica-ci
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 50Gi
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: buildkitd
  namespace: multica-ci
spec:
  replicas: 1
  selector:
    matchLabels: {app: buildkitd}
  strategy:
    type: Recreate
  template:
    metadata:
      labels: {app: buildkitd}
      annotations:
        container.apparmor.security.beta.kubernetes.io/buildkitd: unconfined
    spec:
      containers:
        - name: buildkitd
          image: moby/buildkit:v0.13.0-rootless
          args:
            - --addr
            - unix:///run/user/1000/buildkit/buildkitd.sock
            - --addr
            - tcp://0.0.0.0:1234
            - --oci-worker-no-process-sandbox
          securityContext:
            seccompProfile: {type: Unconfined}
            runAsUser: 1000
            runAsGroup: 1000
          ports:
            - containerPort: 1234
          volumeMounts:
            - name: cache
              mountPath: /home/user/.local/share/buildkit
      volumes:
        - name: cache
          persistentVolumeClaim:
            claimName: buildkit-cache
---
apiVersion: v1
kind: Service
metadata:
  name: buildkitd
  namespace: multica-ci
spec:
  selector: {app: buildkitd}
  ports:
    - port: 1234
      targetPort: 1234
EOF
```

Verify:

```bash
kubectl -n multica-ci rollout status deploy/buildkitd --timeout=2m
kubectl -n multica-ci run buildkit-smoke --rm -it --image=moby/buildkit:v0.13.0 \
  --restart=Never --command -- buildctl --addr tcp://buildkitd.multica-ci.svc:1234 debug info
```

Expected: `buildctl debug info` prints the daemon's version, workers, and platforms.

## Harbor push credentials

The runner pod needs a docker config that authenticates against Harbor:

```bash
kubectl -n arc-runners create secret docker-registry harbor-push \
  --docker-server=registry.chrissnell.com \
  --docker-username=<USER> --docker-password=<PASS>
```

The runner scale set values file mounts this secret into `/home/runner/.docker/config.json`.
