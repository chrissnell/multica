# In-cluster Buildkit

## Why a Deployment, not rootless DaemonSet

We run a single multi-tenant Buildkit Deployment, fronted by a Service the
runner pods address via `tcp://buildkitd.multica-ci.svc:1234`. One daemon
means one shared cache PVC, which is exactly the win we want over
GitHub-hosted runners.

## Note on rootless vs privileged

Rootless buildkit requires the node kernel to expose `user.max_user_namespaces`
non-zero. Talos sets this to 0 by default, so the rootless image fails to
start (`fork/exec /proc/self/exe: no space left on device` from rootlesskit).
We use the non-rootless buildkit image with `privileged: true` instead. The
daemon is contained to the privileged-labeled `multica-ci` namespace and only
serves the in-cluster GHA runners.

## Install

```bash
kubectl create namespace multica-ci
kubectl label ns multica-ci pod-security.kubernetes.io/enforce=privileged --overwrite

cat <<'EOF' | kubectl apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: buildkit-cache
  namespace: multica-ci
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: synology-nfs-csi
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
    spec:
      containers:
        - name: buildkitd
          image: moby/buildkit:v0.13.0
          args:
            - --addr
            - unix:///run/buildkit/buildkitd.sock
            - --addr
            - tcp://0.0.0.0:1234
          securityContext:
            privileged: true
          ports:
            - containerPort: 1234
          volumeMounts:
            - name: cache
              mountPath: /var/lib/buildkit
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
kubectl -n multica-ci run buildkit-smoke --rm -i --image=moby/buildkit:v0.13.0 \
  --restart=Never --overrides='{"spec":{"containers":[{"name":"buildkit-smoke","image":"moby/buildkit:v0.13.0","command":["buildctl","--addr","tcp://buildkitd.multica-ci.svc:1234","debug","info"]}]}}'
```

Expected: prints `BuildKit: github.com/moby/buildkit v0.13.0 ...`.

## Harbor push credentials

The runner pod needs a docker config that authenticates against Harbor:

```bash
kubectl -n arc-runners create secret docker-registry harbor-push \
  --docker-server=registry.chrissnell.com \
  --docker-username=<USER> --docker-password=<PASS>
```

The runner scale set values file mounts this secret into `/home/runner/.docker/config.json`.
