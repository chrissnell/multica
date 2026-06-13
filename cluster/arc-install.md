# ARC (Actions Runner Controller) install

## Prereqs

- `kubectl config current-context` is `admin@farm-talos`.
- `cert-manager` already running in the cluster (ARC depends on it for webhook certs).
- A GitHub App created per `cluster/github-app-setup.md`, with private key on disk.

## Install the controller

```bash
helm upgrade --install arc \
  oci://ghcr.io/actions/actions-runner-controller-charts/gha-runner-scale-set-controller \
  --namespace arc-system --create-namespace
```

Verify:

```bash
kubectl -n arc-system rollout status deploy/arc-gha-rs-controller --timeout=3m
kubectl -n arc-system get pods
```

## Create the runner scale set

The runner pods themselves are installed as a *runner scale set* tied to one GitHub repo (`chrissnell/multica`).

Create a Kubernetes Secret with the GitHub App credentials:

```bash
kubectl create namespace arc-runners
kubectl -n arc-runners create secret generic multica-arc-github-app \
  --from-literal=github_app_id=<APP_ID> \
  --from-literal=github_app_installation_id=<INSTALLATION_ID> \
  --from-file=github_app_private_key=/path/to/private-key.pem
```

Install the runner scale set:

```bash
helm upgrade --install multica-arc-runners \
  oci://ghcr.io/actions/actions-runner-controller-charts/gha-runner-scale-set \
  --namespace arc-runners \
  --set githubConfigUrl=https://github.com/chrissnell/multica \
  --set githubConfigSecret=multica-arc-github-app \
  --set minRunners=0 --set maxRunners=3 \
  --set 'containerMode.type=dind'
```

(Using dind mode lets the runner pod itself construct a Docker context if anything in the pipeline needs one. Buildkit is still the actual builder.)

Verify scale-from-zero:

```bash
kubectl -n arc-runners get pods
```

Expected: no runner pods exist when no jobs are running.

## Smoke test

Create a hello-world workflow on a throwaway branch — see Task 4.3 of `docs/superpowers/plans/2026-06-13-release-pipeline.md`.
