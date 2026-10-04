# DCM Utilities

Common scripts and tooling shared across the [DCM](https://github.com/dcm-project) ecosystem. Provides the E2E deploy script for bringing up the full DCM stack locally and a Ginkgo/Gomega E2E test suite that validates the stack through the control-plane API and DCM CLI.

## Contents

| Path | Description |
|------|-------------|
| `scripts/deploy-dcm.sh` | Deploy, health-check, and tear down the full DCM stack via podman-compose |
| `scripts/kind/` | Kind + compose networking (kubeconfig rewrite, network connect/disconnect) |
| `scripts/compose/` | Compose network teardown (disconnect members, remove networks) |
| `scripts/kubevirt/` | KubeVirt install on any Kubernetes cluster |
| `providers/` | Service provider registry — one `.conf` file per provider |
| `dcm-versions.json` | Example output of container version resolution (gitignored) |
| `tests/run-e2e.sh` | Test harness: deploy, run tests, teardown |
| `tests/e2e/` | Ginkgo/Gomega E2E test suite |
| `test-plans/` | E2E test plans and results for DCM service providers |
| `Makefile` | Convenience targets (`make help` to list all) |

### `dcm-versions.json`

Both deploy mode and `--running-versions` produce a `dcm-versions.json` mapping each container image to its digest and the git commit SHA that built it (resolved via the Quay.io tag API). Third-party images show `null` for `git_sha`.

```json
{
  "quay.io/dcm-project/control-plane:latest": {
    "image_digest": "sha256:1cdf5482f586ce513724074c0a132b718672d2be5cbae600a47e94324078b01e",
    "git_sha": "2388248"
  },
  "docker.io/library/postgres:16-alpine": {
    "image_digest": "sha256:b7587f3cb74f4f4b2a4f9d67f052edbf95eb93f4fec7c5ada3792546caaf7383",
    "git_sha": null
  }
}
```

## E2E Deploy Script

`scripts/deploy-dcm.sh` automates the full DCM stack lifecycle for E2E testing:

1. Clones the [control-plane](https://github.com/dcm-project/control-plane) repo and uses `deploy/compose.yaml`; auth mode also loads `deploy/compose.auth.yaml` with the `auth` profile
2. Bootstraps `deploy/.env` from `deploy/.env.example` (compose credentials; see control-plane `deploy/RUN.md`)
3. Starts the selected Compose model with `podman-compose up`
4. Polls the control-plane health endpoint until it responds 2xx
5. Resolves running container images to git commit SHAs via the Quay.io API

### Prerequisites

- `git`, `podman`, `podman-compose`, `curl`, `jq`
- `oc` (for KubeVirt/ACM providers; also used for `oc login` auth)
- `oc` or `kubectl` (for k8s container and k8s storage providers — either works)
- `oc` + `jq` (for `--deploy-acm` / `--deploy-mce`)

### Quick Start

```bash
# 1. Deploy the full DCM stack (no providers)
./scripts/deploy-dcm.sh

# 2. Deploy a specific release version (auto-derives release branch)
./scripts/deploy-dcm.sh --version v0.1.0-rc.1

# 3. Deploy the latest release (resolves from Quay.io)
./scripts/deploy-dcm.sh --version release

# 4. Deploy with the k8s container service provider (auto-detects cluster)
./scripts/deploy-dcm.sh --k8s-container-service-provider

# 5. Deploy with the k8s storage service provider
./scripts/deploy-dcm.sh --k8s-storage-service-provider --kubeconfig ~/.kube/config

# 6. Deploy with KubeVirt + explicit kubeconfig
./scripts/deploy-dcm.sh --kubevirt-service-provider --kubeconfig ~/.kube/config

# 7. Deploy all providers, logging in via oc
./scripts/deploy-dcm.sh --all-service-providers \
    --cluster-api https://api.cluster.example.com --cluster-password secret

# 8. Deploy ACM cluster provider (install ACM first if needed)
./scripts/deploy-dcm.sh --acm-cluster-service-provider --deploy-acm --kubeconfig ~/.kube/config

# 9. Deploy with auth override and profile (Keycloak + JWT validation)
./scripts/deploy-dcm.sh --auth-enabled

# 10. Deploy with the GitOps reconciliation container
./scripts/deploy-dcm.sh --gitops

# 11. Deploy control-plane + environment-agent with embedded SPs (OCP kubeconfig)
./scripts/deploy-dcm.sh --with-environment-agent \
    --agent-embedded-sps container,vm \
    --kubeconfig ~/.kube/config

# 12. Embed ACM cluster SP via the agent (pull secret resolved from cluster if unset)
./scripts/deploy-dcm.sh --with-environment-agent \
    --agent-embedded-sps cluster \
    --deploy-acm \
    --kubeconfig ~/.kube/config

# 13. Tear down an authenticated stack when done
./scripts/deploy-dcm.sh --auth-enabled --tear-down

# Use --gitops during teardown when the reconciler was enabled
./scripts/deploy-dcm.sh --gitops --tear-down

# Tear down an agent-enabled stack (script detects AGENT_EMBEDDED_SPS in deploy/.env)
./scripts/deploy-dcm.sh --tear-down
```

### Environment agent (embedded SPs)

Prefer the [environment-agent](https://github.com/dcm-project/environment-agent)
compose profile when you want SPs inside one agent process instead of standalone
provider containers. Bring-up is a single `podman-compose` line with
`--profile environment-agent`.

| Item | Detail |
|------|--------|
| Flags | `--with-environment-agent` and `--agent-embedded-sps LIST` (required together) |
| Embedded list | Comma-separated: `container`, `vm`, `cluster`, `storage`, `network` |
| Kubeconfig | Resolved like other providers (`--kubeconfig` / `KUBECONFIG` / session / `oc login`); written to `AGENT_KUBECONFIG_HOST` for the compose bind mount (absolute path; OCP path — no Kind) |
| Agent port | Host API on `--agent-port` / `AGENT_PORT` (default **8081**); same port as standalone KubeVirt SP — combining with `--kubevirt-service-provider` requires a distinct `--agent-port` |
| Health | After CP `/api/v1alpha1/health`, polls agent `http://localhost:${AGENT_PORT}/api/v1alpha1/health` |
| Mutual exclusion | An embedded SP cannot be paired with its overlapping standalone flag (e.g. embedded `container` vs `--k8s-container-service-provider`) |
| Cluster embed | `SP_CLUSTER_NAMESPACE` (default `clusters`) and `SP_PULL_SECRET` (or `ACM_CLUSTER_SP_PULL_SECRET`); if unset, pull secret is read from `openshift-config/pull-secret` |

Useful overrides: `AGENT_EMBEDDED_SPS`, `AGENT_PORT`, `ENVIRONMENT_AGENT_VERSION`, `AGENT_NAME`, `AGENT_ENVIRONMENT`, `AGENT_COST`, `SP_CONTAINER_NAMESPACE`, `SP_VM_NAMESPACE`, `SP_STORAGE_NAMESPACE`, `SP_CLUSTER_NAMESPACE`, `SP_PULL_SECRET`, `SP_BASE_DOMAIN`.

> **Network SP:** embed with `--agent-embedded-sps network` (see agent `deploy/DEPLOY.md`).
> The utilities `--k8s-network-service-provider` flag is **legacy** (standalone Quay
> image path; [FLPATH-4881](https://redhat.atlassian.net/browse/FLPATH-4881) obsolete).
> QE plan: [test-plans/FLPATH-3227-k8s-network-sp.md](test-plans/FLPATH-3227-k8s-network-sp.md).

Run `./scripts/deploy-dcm.sh --help` for all flags and environment variable overrides.

### Podman Compose networking

`deploy-dcm.sh` disables Podman Compose pod mode by default so services use the
Compose bridge network and can resolve each other by service name. To opt into pod
mode for a compatible environment, set the override explicitly:

```bash
PODMAN_COMPOSE_IN_POD=true ./scripts/deploy-dcm.sh
```
The optional `--gitops` flag adds the published `quay.io/dcm-project/dcm-gitops` container
to the Compose stack. The reconciler shares the control-plane PostgreSQL database and
stores cloned repositories in a named `gitops_data` volume. Set `DCM_GITOPS_VERSION` to
pin its image independently, or use `--version` to pin all DCM images together.

## Local dev scripts

Shared helpers for control-plane and environment-agent compose + Kind workflows.

| Path | Description |
|------|-------------|
| [scripts/kind/](scripts/kind/README.md) | Connect Kind to compose and kubeconfig for `https://kubernetes:6443` |
| [scripts/compose/](scripts/compose/README.md) | `network-teardown.sh disconnect` / `remove` around compose down |
| [scripts/kubevirt/](scripts/kubevirt/README.md) | Install KubeVirt on the current `kubectl` context |

## E2E Tests

The test suite uses [Ginkgo](https://onsi.github.io/ginkgo/) and [Gomega](https://onsi.github.io/gomega/) to validate the full DCM stack through the control-plane API and the DCM CLI.

### Quick Start

```bash
# One command: deploy the stack, run all tests, tear down
make test-e2e-full
```

This runs the full lifecycle via `tests/run-e2e.sh`: deploys the DCM stack with `podman-compose`, auto-downloads the CLI binary from GitHub releases, executes all E2E tests (health checks, API CRUD, SP tests, CLI commands), and tears down afterward.

### Step-by-Step

```bash
make e2e-up        # Deploy the stack
make test-e2e      # Run all tests (stack must be running)
make test-smoke    # Run health checks + CLI version only
make test-cli      # Run CLI tests only
make test-sp       # Run container SP tests (SP must be deployed)
make test-acm-sp   # Run ACM cluster SP tests (ACM SP must be deployed)
make test-core     # Run core platform tests (full provisioning flow)
make e2e-down      # Tear down
make download-cli  # Download latest DCM CLI without running tests

# See all targets
make help
```

### Prerequisites

- Go 1.23+
- `podman`, `podman-compose`, `curl`, `jq`, `git`
- `gh` CLI ([cli.github.com](https://cli.github.com)) — for auto-downloading the DCM CLI binary
- **DCM CLI binary** (for CLI tests) — auto-downloaded from [GitHub releases](https://github.com/dcm-project/cli/releases), or set `DCM_CLI_PATH`

### Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `DCM_GATEWAY_URL` | `http://localhost:8080/api/v1alpha1` | Control plane API base URL |
| `DCM_CONTAINER_SP_URL` | `http://localhost:8082/api/v1alpha1` | Container SP direct URL (requires published port) |
| `DCM_STORAGE_SP_URL` | `http://localhost:8089/api/v1alpha1` | Storage SP direct URL (requires published port) |
| `DCM_AGENT_URL` | `http://localhost:8081/api/v1alpha1` | Environment-agent API (`/health`, `/providers`) |
| `DCM_EMBEDDED_SPS` | (none) | Hint list of embedded SPs for capability detection |
| `DCM_NETWORK_SP_ENABLED` | `false` | Force Network SP specs; also auto-enabled when the agent embeds `network` |
| `DCM_KUBEVIRT_SP_URL` | (none) | Standalone KubeVirt SP URL (do not point at the agent on `:8081`) |
| `DCM_ACM_CLUSTER_SP_URL` | `http://localhost:8083/api/v1alpha1` | ACM Cluster SP direct URL (requires published port) |
| `DCM_NATS_URL` | `nats://localhost:4222` | NATS server URL for status event tests |
| `DCM_CLI_PATH` | (auto-resolved) | Path to `dcm` CLI binary |
| `DCM_NETWORK_LB_MODE` | (auto-detect MetalLB) | Network E2E LoadBalancer mode: `none`, `metallb`, or `cloud` |
| `JUNIT_REPORT` | (none) | JUnit XML report filename (e.g. `make test-e2e JUNIT_REPORT=results.xml`) |
| `DCM_AUTH_ENABLED` | `false` | Enable OIDC bearer authentication for API and CLI requests |
| `DCM_AUTH_ISSUER_URL` | (none) | OIDC issuer URL; required when authentication is enabled |
| `DCM_AUTH_CLIENT_ID` | `dcm-proxy` | OIDC client ID |
| `DCM_AUTH_CLIENT_SECRET` | (none) | OIDC client secret for password-grant tokens |
| `DCM_AUTH_USERNAME` | (none) | OIDC user for password-grant tokens |
| `DCM_AUTH_PASSWORD` | (none) | OIDC password for password-grant tokens |
| `DCM_AUTH_TOKEN` | (none) | Optional static bearer token; avoids the password grant |
| `DCM_AUTH_CA_FILE` | (none) | Optional CA bundle for the OIDC issuer |

With `--with-environment-agent`, Ginkgo treats Ready embedded providers from
`GET ${DCM_AGENT_URL}/providers` as capabilities for control-plane tests
(container/vm/cluster/network). Direct standalone SP HTTP suites still require
published SP ports. Network specs also enable automatically when the agent
embeds `network` (or when `DCM_NETWORK_SP_ENABLED=true`).

The network NodePort tests select an unused port after listing Services across
the cluster. The test identity needs permission to list Services in all
namespaces.

### Test Harness Flags

The test harness (`tests/run-e2e.sh`) supports additional flags for fine-grained control:

```bash
./tests/run-e2e.sh --skip-deploy              # Stack is already running
./tests/run-e2e.sh --skip-teardown            # Leave stack running after tests
./tests/run-e2e.sh --skip-cli                 # Skip CLI binary resolution
./tests/run-e2e.sh --dcm-cli-path ~/bin/dcm   # Use a specific CLI binary
./tests/run-e2e.sh --label-filter smoke        # Run only smoke tests
./tests/run-e2e.sh --gateway-url http://...    # Override control plane API URL
./tests/run-e2e.sh --junit-report results.xml  # Write JUnit XML report

# Service provider tests (standalone SP containers)
./tests/run-e2e.sh --k8s-container-service-provider --cluster-api https://api.example.com:6443
./tests/run-e2e.sh --k8s-storage-service-provider --kubeconfig ~/.kube/config
# Environment-agent path (embedded SPs; control-plane + agent tests)
./tests/run-e2e.sh --with-environment-agent --agent-embedded-sps container,vm,network \
  --kubeconfig ~/.kube/config
./tests/run-e2e.sh --skip-deploy --with-environment-agent --agent-embedded-sps network \
  --label-filter "sp && network"
./tests/run-e2e.sh --skip-deploy --label-filter "core && platform"

# Authentication-disabled mode (the default)
./tests/run-e2e.sh --skip-deploy --skip-cli --label-filter smoke

# Authentication-enabled mode against an already deployed RHBK/DCM stack
DCM_AUTH_CLIENT_ID=dcm-proxy \
DCM_AUTH_CLIENT_SECRET="$RHBK_CLIENT_SECRET" \
DCM_AUTH_USERNAME=testuser1 \
DCM_AUTH_PASSWORD="$RHBK_TEST_PASSWORD" \
./tests/run-e2e.sh --skip-deploy --skip-cli \
  --auth-target compose

# Authentication-enabled mode with RHDH/RHBK discovery and disruptive checks
./tests/run-e2e.sh --skip-deploy --skip-cli \
  --auth-target rhdh --auth-disruptive

# --auth-disruptive enables the optional disruptive authentication phase (TC-42/TC-45).
# There is no --auth-advanced flag; use --auth-disruptive for these checks.
# --auth-issuer-url is an optional override when target discovery is unavailable.

# ACM cluster SP tests
./tests/run-e2e.sh --acm-cluster-service-provider --kubeconfig ~/.kube/config
./tests/run-e2e.sh --skip-deploy --label-filter "sp && acm-cluster"
```

The same API and CLI tests run in both modes. Authentication-disabled mode is
the default and sends requests without a bearer token. Authentication-enabled
mode obtains a token from the configured OIDC issuer and uses it for API and
CLI requests. The `auth` label contains authentication boundary checks; those
checks are skipped when authentication is disabled. Keep credentials in the
environment or CI secret store; do not commit them.

For Compose auth discovery, set `DCM_AUTH_KEYCLOAK_CONTAINER` only when the
Keycloak container cannot be found through its Compose service label. The
optional disruptive checks are enabled with `--auth-disruptive`; there is no
`--auth-advanced` runner flag.

Service-provider authentication coverage is separate from the shared suite.
The existing provider tests remain available in authentication-disabled mode.
Provider authentication depends on FLPATH-4622 and should be enabled in the
authenticated run after that support is available.

### Unit Tests

Pure, non-network logic (e.g. `tests/e2e/internal/resolve`) lives outside the `e2e` build tag so it gets real, fast test coverage instead of only being exercised against a live stack:

```bash
cd tests/e2e && go test ./internal/...
```

## Cursor Integration

This repo includes configuration for [Cursor](https://cursor.sh) and [Claude Code](https://claude.ai/code):

| Path | Purpose |
|------|---------|
| `CLAUDE.md` | Consolidated project context (works in any AI tool) |
| `.cursor/rules/` | Auto-loaded context rules for Cursor |
| `.cursor/prompts/` | Task-specific prompt templates (use `@<name>` in Cursor) |
| `.cursor/agents/` | Specialized agent definitions |

Available prompts: `@deploy-dcm`, `@tear-down`, `@check-versions`, `@troubleshoot-deploy`, `@maintain-pr-summary`.

## Development

### Linting

Shell scripts are linted with [ShellCheck](https://www.shellcheck.net/). CI runs ShellCheck automatically on PRs against changed `*.sh` files.

```bash
make lint   # uses local shellcheck, or koalaman/shellcheck via podman/docker
```
## License

Apache 2.0 — see [LICENSE](LICENSE).
