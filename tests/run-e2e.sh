#!/usr/bin/env bash
set -euo pipefail

# DCM E2E Test Harness
# Orchestrates: deploy stack → resolve CLI → run Ginkgo tests → teardown.
# Delegates stack lifecycle to scripts/deploy-dcm.sh.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
readonly REPO_ROOT
readonly DEPLOY_SCRIPT="${REPO_ROOT}/scripts/deploy-dcm.sh"
readonly TEST_DIR="${SCRIPT_DIR}/e2e"
readonly CLI_BIN_DIR="${REPO_ROOT}/bin"
readonly CLI_GITHUB_REPO="dcm-project/cli"

# --- Usage ----------------------------------------------------------------- #

usage() {
    cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Run the DCM E2E test suite. By default, deploys the stack, runs all tests,
and tears down afterward.

Options:
  --skip-deploy                Skip stack deployment (assumes stack is running)
  --skip-teardown              Leave the stack running after tests
  --skip-cli                   Skip CLI binary resolution (CLI tests will be skipped)
  --dcm-cli-path PATH          Path to pre-built dcm binary (skips resolution)
  --auth-enabled               Enable RHBK/OIDC bearer authentication for API and CLI tests
  --auth-issuer-url URL        Override the discovered OIDC issuer URL
  --auth-target TARGET         Authentication target: rhdh or compose
  --auth-namespace NS          RHDH namespace used to discover auth settings
  --auth-ca-file PATH          CA bundle used to connect to RHBK
  --auth-disruptive            Prepare and run disruptive authenticated tests (TC-42 and TC-45)
  --gateway-url URL            Override DCM_GATEWAY_URL (default: http://localhost:8080/api/v1alpha1)
  --label-filter EXPR          Ginkgo label filter (e.g. "smoke", "cli")
  --junit-report FILE          Write JUnit XML report to FILE
  --with-environment-agent     Enable environment-agent (forwards to deploy; exports DCM_AGENT_URL)
  --agent-embedded-sps LIST    Embedded SPs for the agent (e.g. container,vm,network)
  --agent-port PORT            Host port for environment-agent API (default: 8081)
  --help                       Show this help message

Deploy passthrough flags (forwarded to deploy-dcm.sh):
  --control-plane-branch REF     Branch to clone
  --control-plane-dir PATH       Directory to clone into
  --control-plane-repo URL       Git repo for control-plane
  --cleanup-on-failure         Tear down on deployment failure
  --gitops                      Enable the dcm-gitops reconciliation container

Service provider flags (forwarded to deploy-dcm.sh):
  --all-service-providers           Enable all SPs
  --k8s-container-service-provider  Enable the k8s container SP (standalone)
  --k8s-storage-service-provider    Enable the k8s storage SP
  --kubevirt-service-provider       Enable the kubevirt SP (standalone)
  --acm-cluster-service-provider    Enable the ACM cluster SP (standalone)
  --deploy-acm                      Deploy ACM on the cluster (opt-in, heavy)
  --deploy-mce                      Deploy MCE on the cluster (opt-in, heavy)
  --deploy-cnv                      Deploy CNV on the cluster (opt-in, heavy)
  --kubeconfig PATH                 Path to kubeconfig file
  --k8s-container-namespace NS      Namespace for container workloads
  --k8s-storage-namespace NS        Namespace for storage PVCs
  --acm-cluster-namespace NS        Namespace for ACM clusters
  --kubevirt-vm-namespace NS        Namespace for kubevirt VMs
  --cluster-api URL                 OpenShift API URL for oc login
  --cluster-username USER           Username for oc login
  --cluster-password PASS           Password for oc login

Environment variables:
  DCM_AGENT_URL            Environment-agent API URL (default: http://localhost:8081/api/v1alpha1)
  DCM_EMBEDDED_SPS         Optional discovery hint of embedded SPs (not readiness evidence); live /providers Ready types are merged for deploy awareness
  DCM_NETWORK_SP_ENABLED   Require the embedded Network SP (default: false; auto true when network is embedded)
  DCM_CONTAINER_SP_URL     Container SP direct URL (default: http://localhost:8082/api/v1alpha1)
  DCM_STORAGE_SP_URL       Storage SP direct URL (default: http://localhost:8089/api/v1alpha1)
  DCM_ACM_CLUSTER_SP_URL   ACM Cluster SP direct URL (default: http://localhost:8083/api/v1alpha1)
  DCM_KUBEVIRT_SP_URL      KubeVirt SP direct URL (do not default to :8081 when the agent owns that port)
  DCM_NATS_URL             NATS URL for event tests (default: nats://localhost:4222)
  DCM_GATEWAY_URL          Control plane API URL (default: http://localhost:8080/api/v1alpha1)
  DCM_AUTH_ENABLED         Enable OIDC bearer authentication (default: false)
  DCM_AUTH_ISSUER_URL      OIDC issuer URL override (normally discovered during the run)
  DCM_AUTH_TOKEN_ISSUER_URL Optional token endpoint base URL when the issuer is only resolvable inside Compose
  DCM_AUTH_CLIENT_ID       OIDC client ID (default: dcm-proxy)
  DCM_AUTH_CLIENT_SECRET   OIDC client secret
  DCM_AUTH_USERNAME        OIDC user name for password-grant tokens
  DCM_AUTH_PASSWORD        OIDC password for password-grant tokens
  DCM_AUTH_TOKEN           Optional static bearer token (avoids password grant)
  DCM_AUTH_CA_FILE         Optional CA bundle for the OIDC issuer
  DCM_AUTH_RESTART_COMMAND Command used by TC-42 to restart the auth provider
  DCM_AUTH_PROXY_URL       RHDH DCM proxy URL for TC-43
  DCM_AUTH_PROXY_SESSION_TOKEN  Valid RHDH session token for TC-43
  DCM_AUTH_ADMIN_URL       RHBK realm admin API URL for TC-45
  DCM_AUTH_ADMIN_TOKEN     RHBK admin bearer token for TC-45
  DCM_AUTH_JWKS_URL        Host-reachable JWKS URL when discovery uses an internal Compose hostname
  DCM_AUTH_KEYCLOAK_CONTAINER  Compose Keycloak container override for auth discovery
  DCM_AUTH_DCM_LOG_COMMAND  Command returning DCM logs to inspect for TC-46
  DCM_AUTH_RHDH_LOG_COMMAND Command returning RHDH logs to inspect for TC-46

CLI binary resolution order:
  1. --dcm-cli-path flag or DCM_CLI_PATH env var
  2. dcm in \$PATH
  3. Previously downloaded binary in bin/dcm
  4. Auto-download latest release from GitHub (requires gh CLI)

Examples:
  $(basename "$0")
  $(basename "$0") --skip-deploy
  $(basename "$0") --skip-deploy --label-filter smoke
  $(basename "$0") --dcm-cli-path ~/git/dcm/cli/bin/dcm
  $(basename "$0") --skip-cli --label-filter '!cli'
  $(basename "$0") --control-plane-branch feature-x --skip-teardown
  $(basename "$0") --k8s-container-service-provider --cluster-api https://api.example.com:6443
  $(basename "$0") --skip-deploy --label-filter "sp && container"
EOF
}

# --- Logging --------------------------------------------------------------- #

log()  { echo "==> $*"; }
info() { echo "    $*"; }
warn() { echo "WARNING: $*" >&2; }
err()  { echo "ERROR: $*" >&2; }

# --- CLI binary resolution ------------------------------------------------- #

download_dcm_cli() {
    local version="${1:-main}"
    local detected_os detected_arch

    if ! command -v gh &>/dev/null; then
        err "gh CLI not found — cannot auto-download DCM CLI"
        err "Install gh (https://cli.github.com) or provide --dcm-cli-path"
        return 1
    fi

    detected_os="$(uname -s | tr '[:upper:]' '[:lower:]')"
    detected_arch="$(uname -m)"
    case "${detected_arch}" in
        x86_64)  detected_arch="amd64" ;;
        aarch64) detected_arch="arm64" ;;
    esac

    mkdir -p "${CLI_BIN_DIR}"
    log "Downloading DCM CLI (${version}) for ${detected_os}/${detected_arch}"
    gh release download "${version}" --repo "${CLI_GITHUB_REPO}" --pattern "cli_*_${detected_os}_${detected_arch}.tar.gz" --dir "${CLI_BIN_DIR}" --clobber
    tar -xzf "${CLI_BIN_DIR}"/cli_*_"${detected_os}"_"${detected_arch}".tar.gz -C "${CLI_BIN_DIR}" dcm
    rm -f "${CLI_BIN_DIR}"/cli_*_"${detected_os}"_"${detected_arch}".tar.gz
    chmod +x "${CLI_BIN_DIR}/dcm"
    info "Downloaded to ${CLI_BIN_DIR}/dcm"
}

log_cli_version() {
    local cli_version_output
    cli_version_output="$("${DCM_CLI_PATH}" version 2>&1)" || return 0
    local ver commit
    ver="$(echo "${cli_version_output}" | awk '/^dcm version/{sub(/^dcm version /,""); print}')"
    commit="$(echo "${cli_version_output}" | awk '/commit:/{sub(/^ *commit: */,""); print}')"
    info "DCM CLI ${ver} (commit ${commit})"
}

resolve_dcm_cli() {
    # 1. Explicit path (flag or env var).
    if [[ -n "${DCM_CLI_PATH}" ]]; then
        if [[ ! -x "${DCM_CLI_PATH}" ]]; then
            err "DCM CLI not found or not executable: ${DCM_CLI_PATH}"
            return 1
        fi
        info "Using DCM CLI: ${DCM_CLI_PATH}"
        log_cli_version
        return 0
    fi

    # 2. Remove any stale binary, then download fresh.
    rm -f "${CLI_BIN_DIR}/dcm"
    if download_dcm_cli "${CLI_VERSION}"; then
        DCM_CLI_PATH="${CLI_BIN_DIR}/dcm"
        log_cli_version
        return 0
    fi

    err "Could not download DCM CLI — CLI tests will be skipped"
    return 1
}

# --- Argument parsing ------------------------------------------------------ #

SKIP_DEPLOY=false
SKIP_TEARDOWN=false
SKIP_CLI=false
DCM_CLI_PATH="${DCM_CLI_PATH:-}"
CLI_VERSION="${CLI_VERSION:-main}"
GATEWAY_URL=""
AUTH_ENABLED="${DCM_AUTH_ENABLED:-false}"
AUTH_ISSUER_URL="${DCM_AUTH_ISSUER_URL:-}"
AUTH_TOKEN_ISSUER_URL="${DCM_AUTH_TOKEN_ISSUER_URL:-}"
AUTH_ISSUER_EXPLICIT=false
if [[ -n "${AUTH_ISSUER_URL}" ]]; then
    AUTH_ISSUER_EXPLICIT=true
fi
AUTH_TARGET="${DCM_AUTH_TARGET:-}"
AUTH_NAMESPACE="${DCM_AUTH_NAMESPACE:-${RHDH_NAMESPACE:-rhdh-operator}}"
AUTH_CA_FILE="${DCM_AUTH_CA_FILE:-}"
AUTH_DISRUPTIVE="${DCM_AUTH_DISRUPTIVE:-false}"
CONTROL_PLANE_DIR="${CONTROL_PLANE_TMP_DIR:-/tmp/dcm-e2e}"
LABEL_FILTER=""
JUNIT_REPORT=""
DEPLOY_ARGS=()
ENABLE_CONTAINER_SP=false
ENABLE_ACM_CLUSTER_SP=false
ENABLE_KUBEVIRT_SP=false
WITH_ENVIRONMENT_AGENT=false
AGENT_EMBEDDED_SPS="${AGENT_EMBEDDED_SPS:-${DCM_EMBEDDED_SPS:-}}"
AGENT_PORT="${AGENT_PORT:-8081}"
KUBEVIRT_VM_NS_ARG=""

agent_list_contains() {
    local needle="$1"
    local norm
    norm="$(printf '%s' "${AGENT_EMBEDDED_SPS}" | tr -d '[:space:]')"
    case ",${norm}," in
        *,"${needle}",*) return 0 ;;
        *) return 1 ;;
    esac
}

# Merge comma-separated SP tokens (lowercase, de-duplicated, first-seen order).
merge_embedded_sps() {
    python3 -c '
import sys
seen = []
for raw in ",".join(sys.argv[1:]).split(","):
    tok = raw.strip().lower()
    if tok and tok not in seen:
        seen.append(tok)
print(",".join(seen))
' "${1:-}" "${2:-}"
}

# Ready service_type values from a live environment-agent /providers list.
# Only status=Ready counts — deploy hints in DCM_EMBEDDED_SPS / AGENT_EMBEDDED_SPS
# must not be treated as readiness evidence here.
fetch_ready_embedded_sps() {
    local url="$1"
    curl -sf --connect-timeout 2 --max-time 5 "${url}/providers" 2>/dev/null \
        | python3 -c '
import json, sys
try:
    data = json.load(sys.stdin)
except Exception:
    raise SystemExit(0)
out = []
for p in data.get("results") or []:
    st = (p.get("service_type") or "").strip().lower()
    status = (p.get("status") or "").lower()
    if st and status == "ready" and st not in out:
        out.append(st)
print(",".join(out))
' 2>/dev/null || true
}

# Detect a running agent and union its Ready providers into AGENT_EMBEDDED_SPS
# even when DCM_EMBEDDED_SPS / --agent-embedded-sps / ENABLE_* were omitted
# or only listed a subset (Jenkins often passes network,storage alone).
# LIVE_READY_EMBEDDED_SPS is the Ready-only snapshot for suite enablement.
LIVE_READY_EMBEDDED_SPS=""
sync_environment_agent_from_live() {
    export DCM_AGENT_URL="${DCM_AGENT_URL:-http://localhost:${AGENT_PORT}/api/v1alpha1}"
    LIVE_READY_EMBEDDED_SPS=""
    if ! curl -sf --connect-timeout 2 --max-time 5 "${DCM_AGENT_URL}/health" >/dev/null 2>&1; then
        return 0
    fi
    if [[ "${WITH_ENVIRONMENT_AGENT}" != "true" ]]; then
        WITH_ENVIRONMENT_AGENT=true
        info "Detected live environment agent at ${DCM_AGENT_URL}"
    fi
    LIVE_READY_EMBEDDED_SPS="$(fetch_ready_embedded_sps "${DCM_AGENT_URL}")"
    if [[ -z "${LIVE_READY_EMBEDDED_SPS}" ]]; then
        return 0
    fi
    info "Live agent Ready providers: ${LIVE_READY_EMBEDDED_SPS}"
    AGENT_EMBEDDED_SPS="$(merge_embedded_sps "${AGENT_EMBEDDED_SPS}" "${LIVE_READY_EMBEDDED_SPS}")"
}

live_ready_contains() {
    local needle="$1"
    local norm
    norm="$(printf '%s' "${LIVE_READY_EMBEDDED_SPS}" | tr -d '[:space:]')"
    case ",${norm}," in
        *,"${needle}",*) return 0 ;;
        *) return 1 ;;
    esac
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --skip-deploy)
            SKIP_DEPLOY=true
            shift ;;
        --skip-teardown)
            SKIP_TEARDOWN=true
            shift ;;
        --skip-cli)
            SKIP_CLI=true
            shift ;;
        --dcm-cli-path)
            DCM_CLI_PATH="$2"
            shift 2 ;;
        --auth-enabled)
            AUTH_ENABLED=true
            shift ;;
        --auth-issuer-url|--keycloak-url)
            AUTH_ENABLED=true
            AUTH_ISSUER_URL="$2"
            AUTH_ISSUER_EXPLICIT=true
            shift 2 ;;
        --auth-target)
            AUTH_TARGET="$2"
            shift 2 ;;
        --auth-namespace)
            AUTH_NAMESPACE="$2"
            shift 2 ;;
        --auth-ca-file)
            AUTH_CA_FILE="$2"
            shift 2 ;;
        --auth-disruptive)
            AUTH_DISRUPTIVE=true
            shift ;;
        --cli-version)
            CLI_VERSION="$2"
            shift 2 ;;
        --gateway-url)
            GATEWAY_URL="$2"
            shift 2 ;;
        --label-filter)
            LABEL_FILTER="$2"
            shift 2 ;;
        --junit-report)
            JUNIT_REPORT="$2"
            shift 2 ;;
        --control-plane-dir)
            CONTROL_PLANE_DIR="$2"
            DEPLOY_ARGS+=("$1" "$2")
            shift 2 ;;
        --with-environment-agent)
            WITH_ENVIRONMENT_AGENT=true
            DEPLOY_ARGS+=("$1")
            shift ;;
        --agent-embedded-sps)
            WITH_ENVIRONMENT_AGENT=true
            AGENT_EMBEDDED_SPS="$2"
            DEPLOY_ARGS+=("$1" "$2")
            shift 2 ;;
        --agent-port)
            AGENT_PORT="$2"
            DEPLOY_ARGS+=("$1" "$2")
            shift 2 ;;
        --control-plane-branch|--control-plane-repo)
            DEPLOY_ARGS+=("$1" "$2")
            shift 2 ;;
        --cleanup-on-failure)
            DEPLOY_ARGS+=("$1")
            shift ;;
        --gitops)
            DEPLOY_ARGS+=("$1")
            shift ;;
        --k8s-container-service-provider)
            ENABLE_CONTAINER_SP=true
            DEPLOY_ARGS+=("$1")
            shift ;;
        --all-service-providers)
            ENABLE_CONTAINER_SP=true
            ENABLE_ACM_CLUSTER_SP=true
            ENABLE_KUBEVIRT_SP=true
            DEPLOY_ARGS+=("$1")
            shift ;;
        --acm-cluster-service-provider)
            ENABLE_ACM_CLUSTER_SP=true
            DEPLOY_ARGS+=("$1")
            shift ;;
        --kubevirt-service-provider)
            ENABLE_KUBEVIRT_SP=true
            DEPLOY_ARGS+=("$1")
            shift ;;
        --deploy-acm|--deploy-mce|--deploy-cnv)
            DEPLOY_ARGS+=("$1")
            shift ;;
        --compose-file|--kubeconfig|--k8s-container-namespace|--acm-cluster-namespace|--cluster-api|--cluster-username|--cluster-password|--acm-cluster-sp-repo|--acm-cluster-sp-branch)
            DEPLOY_ARGS+=("$1" "$2")
            shift 2 ;;
        --kubevirt-vm-namespace)
            KUBEVIRT_VM_NS_ARG="$2"
            DEPLOY_ARGS+=("$1" "$2")
            shift 2 ;;
        --help)
            usage
            exit 0 ;;
        *)
            err "Unknown option: $1"
            usage
            exit 1 ;;
    esac
done

if [[ -n "${AGENT_EMBEDDED_SPS}" ]]; then
    WITH_ENVIRONMENT_AGENT=true
fi
# When deploying (not --skip-deploy), agent mode needs an embedded list and
# must forward agent flags to deploy-dcm.sh (including env-only configuration).
if [[ "${WITH_ENVIRONMENT_AGENT}" == "true" && "${SKIP_DEPLOY}" == "false" ]]; then
    if [[ -z "${AGENT_EMBEDDED_SPS}" ]]; then
        err "--with-environment-agent requires --agent-embedded-sps (or AGENT_EMBEDDED_SPS / DCM_EMBEDDED_SPS) when deploying"
        exit 1
    fi
    # Avoid duplicating flags if the user already passed them on the CLI.
    if [[ " ${DEPLOY_ARGS[*]} " != *" --with-environment-agent "* ]]; then
        DEPLOY_ARGS+=(--with-environment-agent)
    fi
    if [[ " ${DEPLOY_ARGS[*]} " != *" --agent-embedded-sps "* ]]; then
        DEPLOY_ARGS+=(--agent-embedded-sps "${AGENT_EMBEDDED_SPS}")
    fi
fi

# Resolve the VM namespace used by the embedded provider (SP_VM_NAMESPACE) and
# by Ginkgo cluster lookups (KUBERNETES_NAMESPACE / KUBEVIRT_VM_NAMESPACE).
# Must run before deploy so deploy-dcm.sh writes the same value into deploy/.env.
resolve_embedded_vm_namespace() {
    [[ "${WITH_ENVIRONMENT_AGENT}" == "true" ]] || return 0
    agent_list_contains vm || return 0
    local vm_ns="${SP_VM_NAMESPACE:-${KUBEVIRT_VM_NS_ARG:-${KUBEVIRT_VM_NAMESPACE:-default}}}"
    export SP_VM_NAMESPACE="${vm_ns}"
    export KUBERNETES_NAMESPACE="${KUBERNETES_NAMESPACE:-${vm_ns}}"
    export KUBEVIRT_VM_NAMESPACE="${vm_ns}"
    info "Embedded VM namespace: SP_VM_NAMESPACE=${SP_VM_NAMESPACE} (lookups: KUBERNETES_NAMESPACE=${KUBERNETES_NAMESPACE})"
}
resolve_embedded_vm_namespace

# --- Main ------------------------------------------------------------------ #

prepare_auth_test_settings() {
    local issuer_base realm route_host user_response refresh_token admin_password
    local admin_token
    local -a ca_args=()

    [[ "${AUTH_ENABLED}" == true ]] || return 0
    command -v curl >/dev/null || { err "curl is required for authenticated tests"; return 1; }
    command -v jq >/dev/null || { err "jq is required for authenticated tests"; return 1; }
    if [[ -n "${AUTH_CA_FILE}" ]]; then
        ca_args=(--cacert "${AUTH_CA_FILE}")
    fi

    if [[ "${AUTH_TARGET}" == rhdh ]]; then
        route_host=""
        if command -v oc >/dev/null; then
            route_host="$(oc -n "${AUTH_NAMESPACE}" get route -o jsonpath='{.items[0].spec.host}' 2>/dev/null || true)"
        fi
        issuer_base="${AUTH_ISSUER_URL%/realms/*}"
        realm="${AUTH_ISSUER_URL##*/realms/}"
        if [[ -n "${route_host}" ]]; then
            if user_response="$(curl --fail --silent --show-error \
                "${ca_args[@]}" \
                -X POST "${AUTH_ISSUER_URL}/protocol/openid-connect/token" \
                -d grant_type=password -d client_id="${DCM_AUTH_CLIENT_ID}" \
                --data-urlencode client_secret="${DCM_AUTH_CLIENT_SECRET}" \
                -d username="${DCM_AUTH_USERNAME}" -d password="${DCM_AUTH_PASSWORD}" -d scope=openid)" \
                && refresh_token="$(printf '%s' "${user_response}" | jq -er .refresh_token)" \
                && DCM_AUTH_PROXY_SESSION_TOKEN="$(curl --fail --silent --show-error \
                    "${ca_args[@]}" \
                    "https://${route_host}/api/auth/oidc/refresh?optional&scope=openid%20profile%20email&env=production" \
                    -H 'x-requested-with: XMLHttpRequest' \
                    --cookie "oidc-refresh-token=${refresh_token}" | jq -er .backstageIdentity.token)"; then
                export DCM_AUTH_PROXY_URL="https://${route_host}/api/dcm/proxy"
                export DCM_AUTH_PROXY_SESSION_TOKEN
            else
                warn "RHDH proxy session setup failed; TC-43 and proxy checks will skip"
            fi
        else
            warn "RHDH route is unavailable; TC-43 will skip"
        fi
        if command -v oc >/dev/null \
            && admin_password="$(oc -n "${RHBK_NAMESPACE:-rhbk}" get secret rhbk-admin -o jsonpath='{.data.password}' 2>/dev/null | base64 -d)" \
            && admin_token="$(curl --fail --silent --show-error "${ca_args[@]}" \
                -X POST "${issuer_base}/realms/master/protocol/openid-connect/token" \
                -d grant_type=password -d client_id=admin-cli -d username=admin \
                --data-urlencode password="${admin_password}" | jq -er .access_token)"; then
            export DCM_AUTH_ADMIN_TOKEN="${admin_token}"
        else
            warn "RHBK admin token setup failed; TC-44 and TC-45 will skip"
        fi
        export DCM_AUTH_ADMIN_URL="${issuer_base}/admin/realms/${realm}"
        if command -v oc >/dev/null; then
            export DCM_AUTH_RESTART_COMMAND="oc -n ${RHBK_NAMESPACE:-rhbk} rollout restart statefulset/rhbk"
            export DCM_AUTH_RHDH_LOG_COMMAND="oc -n ${AUTH_NAMESPACE} logs -l app.kubernetes.io/name=backstage --all-containers=true"
        fi
        if [[ -z "${DCM_AUTH_DCM_LOG_COMMAND:-}" ]]; then
            warn "DCM_AUTH_DCM_LOG_COMMAND is not set for the RHDH target; TC-46 will skip DCM log checks"
        fi
        return 0
    fi

    if [[ "${AUTH_TARGET}" == compose ]]; then
        issuer_base="${DCM_AUTH_TOKEN_ISSUER_URL:-${AUTH_ISSUER_URL}}"
        export DCM_AUTH_JWKS_URL="${DCM_AUTH_JWKS_URL:-${issuer_base}/protocol/openid-connect/certs}"
        realm="${AUTH_ISSUER_URL##*/realms/}"
        if [[ "${realm}" == "${AUTH_ISSUER_URL}" || -z "${realm}" ]]; then
            realm="${AUTH_REALM:-dcm}"
        fi
        issuer_base="${issuer_base%/realms/*}"
        export DCM_AUTH_ADMIN_URL="${DCM_AUTH_ADMIN_URL:-${issuer_base}/admin/realms/${realm}}"
        if [[ -n "${DCM_AUTH_ADMIN_USERNAME:-}" && -n "${DCM_AUTH_ADMIN_PASSWORD:-}" ]]; then
            admin_token="$(curl --fail --silent --show-error \
                "${ca_args[@]}" \
                -X POST "${issuer_base}/realms/master/protocol/openid-connect/token" \
                -d grant_type=password -d client_id=admin-cli \
                --data-urlencode username="${DCM_AUTH_ADMIN_USERNAME}" \
                --data-urlencode password="${DCM_AUTH_ADMIN_PASSWORD}" | jq -er .access_token)"
            export DCM_AUTH_ADMIN_TOKEN="${admin_token}"
        fi
        export DCM_AUTH_RESTART_COMMAND="${DCM_AUTH_RESTART_COMMAND:-podman restart dcm-e2e_keycloak_1}"
        export DCM_AUTH_DCM_LOG_COMMAND="${DCM_AUTH_DCM_LOG_COMMAND:-podman logs dcm-e2e_control-plane_1}"
        return 0
    fi

    if [[ "${AUTH_DISRUPTIVE}" == true ]]; then
        err "--auth-disruptive requires --auth-target rhdh or compose"
        return 1
    fi
    return 0
}

read_deploy_env_value() {
    local key="$1" env_file="$2"
    sed -n "s/^${key}=//p" "${env_file}" | tail -n 1
}

discover_compose_auth() {
    local env_file="${CONTROL_PLANE_DIR}/deploy/.env"
    local internal_issuer issuer_path keycloak_container mapped_port

    internal_issuer="$(read_deploy_env_value AUTH_ISSUER_URL "${env_file}")"
    if [[ -z "${internal_issuer}" ]]; then
        err "AUTH_ISSUER_URL is missing from ${env_file}"
        return 1
    fi
    if [[ "${AUTH_ISSUER_EXPLICIT}" != true ]]; then
        AUTH_ISSUER_URL="${internal_issuer}"
    fi

    if [[ -z "${AUTH_TOKEN_ISSUER_URL}" && "${internal_issuer}" == *://keycloak:*/* ]]; then
        command -v podman >/dev/null || { err "podman is required to discover the Compose auth endpoint"; return 1; }
        keycloak_container="${DCM_AUTH_KEYCLOAK_CONTAINER:-}"
        if [[ -z "${keycloak_container}" ]]; then
            keycloak_container="$(podman ps \
                --filter label=com.docker.compose.service=keycloak \
                --format '{{.Names}}' | head -n 1)"
        fi
        if [[ -z "${keycloak_container}" ]]; then
            keycloak_container="$(podman ps \
                --filter label=io.podman.compose.service=keycloak \
                --format '{{.Names}}' | head -n 1)"
        fi
        if [[ -z "${keycloak_container}" ]]; then
            err "Unable to discover the Compose Keycloak container; set DCM_AUTH_KEYCLOAK_CONTAINER"
            return 1
        fi
        mapped_port="$(podman port "${keycloak_container}" 8080/tcp | head -n 1 | awk -F: '{print $NF}')"
        if [[ -z "${mapped_port}" ]]; then
            err "Unable to discover the host port for ${keycloak_container}"
            return 1
        fi
        issuer_path="${internal_issuer#*://}"
        issuer_path="/${issuer_path#*/}"
        AUTH_TOKEN_ISSUER_URL="http://127.0.0.1:${mapped_port}${issuer_path}"
    elif [[ -z "${AUTH_TOKEN_ISSUER_URL}" ]]; then
        AUTH_TOKEN_ISSUER_URL="${internal_issuer}"
    fi

    export DCM_AUTH_ISSUER_URL="${AUTH_ISSUER_URL}"
    export DCM_AUTH_TOKEN_ISSUER_URL="${AUTH_TOKEN_ISSUER_URL}"
    export AUTH_ISSUER_URL="${AUTH_TOKEN_ISSUER_URL}"
    info "Compose auth issuer discovered (internal=${DCM_AUTH_ISSUER_URL}, token=${DCM_AUTH_TOKEN_ISSUER_URL})"
}

prepare_compose_auth() {
    local env_file="${CONTROL_PLANE_DIR}/deploy/.env"

    [[ "${AUTH_ENABLED}" == true && "${AUTH_TARGET}" == compose ]] || return 0
    if [[ ! -f "${env_file}" ]]; then
        err "Compose auth environment not found: ${env_file}"
        return 1
    fi

    export DCM_AUTH_CLIENT_ID="${DCM_AUTH_CLIENT_ID:-dcm-proxy}"
    export DCM_AUTH_CLIENT_SECRET="${DCM_AUTH_CLIENT_SECRET:-$(read_deploy_env_value AUTH_PROXY_SECRET "${env_file}")}"
    export DCM_AUTH_USERNAME="${DCM_AUTH_USERNAME:-dcm-admin}"
    export DCM_AUTH_PASSWORD="${DCM_AUTH_PASSWORD:-$(read_deploy_env_value DCM_DEV_USER_PASSWORD "${env_file}")}"
    export DCM_AUTH_ADMIN_USERNAME="${DCM_AUTH_ADMIN_USERNAME:-$(read_deploy_env_value KEYCLOAK_ADMIN "${env_file}")}"
    export DCM_AUTH_ADMIN_PASSWORD="${DCM_AUTH_ADMIN_PASSWORD:-$(read_deploy_env_value KEYCLOAK_ADMIN_PASSWORD "${env_file}")}"
}

if ! command -v go &>/dev/null; then
    err "Go toolchain not found — install Go before running tests"
    exit 1
fi

if [[ "${AUTH_ENABLED}" == "true" ]]; then
    if [[ "${AUTH_TARGET}" == rhdh && -z "${AUTH_ISSUER_URL}" ]]; then
        AUTH_ISSUER_URL="$(oc -n "${AUTH_NAMESPACE}" get configmap rhbk-dcm-auth -o jsonpath='{.data.issuer-url}')"
    fi
    if [[ "${AUTH_TARGET}" == compose && -z "${AUTH_ISSUER_URL}" ]]; then
        AUTH_ISSUER_URL="http://keycloak:8080/realms/dcm"
    fi
    if [[ -z "${AUTH_ISSUER_URL}" ]]; then
        err "Unable to discover the OIDC issuer; use --auth-issuer-url as an override"
        exit 1
    fi
    if [[ "${AUTH_TARGET}" == rhdh ]]; then
        export DCM_AUTH_CLIENT_ID="${DCM_AUTH_CLIENT_ID:-rhdh-auth}"
        if [[ -z "${DCM_AUTH_CLIENT_SECRET:-}" ]]; then
            DCM_AUTH_CLIENT_SECRET="$(oc -n "${AUTH_NAMESPACE}" get secret rhdh-auth-secrets -o jsonpath='{.data.KEYCLOAK_CLIENT_SECRET}' | base64 -d)"
            export DCM_AUTH_CLIENT_SECRET
        fi
        export DCM_AUTH_USERNAME="${DCM_AUTH_USERNAME:-testuser1}"
        export DCM_AUTH_PASSWORD="${DCM_AUTH_PASSWORD:-testuser1}"
    fi
    if [[ -n "${AUTH_CA_FILE}" ]]; then
        export DCM_AUTH_CA_FILE="${AUTH_CA_FILE}"
    fi
    export DCM_AUTH_ENABLED=true
    export DCM_AUTH_ISSUER_URL="${AUTH_ISSUER_URL}"
    if [[ -n "${AUTH_TOKEN_ISSUER_URL}" ]]; then
        export DCM_AUTH_TOKEN_ISSUER_URL="${AUTH_TOKEN_ISSUER_URL}"
    fi
    export DCM_AUTH_AUDIENCE="${DCM_AUTH_AUDIENCE:-dcm-api}"
    export AUTH_ISSUER_URL="${AUTH_ISSUER_URL}"
    DEPLOY_ARGS+=(--auth-enabled)
    info "DCM authentication enabled for E2E requests"
else
    export DCM_AUTH_ENABLED=false
    info "DCM authentication disabled for E2E requests"
fi

# Deploy the stack.
if [[ "${SKIP_DEPLOY}" == "false" ]]; then
    log "Deploying DCM stack"
    "${DEPLOY_SCRIPT}" "${DEPLOY_ARGS[@]+"${DEPLOY_ARGS[@]}"}"
else
    log "Skipping deployment (--skip-deploy)"
fi

prepare_compose_auth
if [[ "${AUTH_ENABLED}" == true && "${AUTH_TARGET}" == compose ]]; then
    discover_compose_auth
fi

# Prepare settings needed by both regular and disruptive authentication tests.
# This must happen before the regular suite so TC-43, TC-44, and TC-46 do not
# silently skip because their proxy, admin, or log settings were not exported.
prepare_auth_test_settings

# Resolve CLI binary.
if [[ "${SKIP_CLI}" == "false" ]]; then
    if resolve_dcm_cli; then
        export DCM_CLI_PATH
        info "DCM_CLI_PATH=${DCM_CLI_PATH}"
    fi
else
    log "Skipping CLI resolution (--skip-cli)"
fi

# Export gateway URL if provided.
if [[ -n "${GATEWAY_URL}" ]]; then
    export DCM_GATEWAY_URL="${GATEWAY_URL}"
    info "DCM_GATEWAY_URL=${GATEWAY_URL}"
fi

# Probe a live agent after the stack is up so Ready /providers types are known
# even without DCM_EMBEDDED_SPS, --agent-embedded-sps, or ENABLE_* toggles.
sync_environment_agent_from_live
if [[ "${WITH_ENVIRONMENT_AGENT}" == "true" ]]; then
    # Discovery hint for Ginkgo (deployed/requested embeds). Readiness is decided
    # by live /providers inside the tests — do not treat this as Ready evidence.
    export DCM_EMBEDDED_SPS="${AGENT_EMBEDDED_SPS}"
    info "DCM_AGENT_URL=${DCM_AGENT_URL}"
    info "DCM_EMBEDDED_SPS=${DCM_EMBEDDED_SPS:-} (discovery hint; Ready=${LIVE_READY_EMBEDDED_SPS:-none})"
    # Auto-enable the network suite only from live Ready (or an explicit prior set).
    if [[ "${DCM_NETWORK_SP_ENABLED:-}" == "true" ]] || live_ready_contains network; then
        export DCM_NETWORK_SP_ENABLED=true
        info "DCM_NETWORK_SP_ENABLED=true"
    fi
    export DCM_NATS_URL="${DCM_NATS_URL:-nats://localhost:4222}"
    info "DCM_NATS_URL=${DCM_NATS_URL}"
fi

# Export SP URLs when standalone providers are enabled.
if [[ "${ENABLE_CONTAINER_SP}" == "true" ]] || [[ "${ENABLE_ACM_CLUSTER_SP}" == "true" ]] || [[ "${ENABLE_KUBEVIRT_SP}" == "true" ]]; then
    export DCM_NATS_URL="${DCM_NATS_URL:-nats://localhost:4222}"
    info "DCM_NATS_URL=${DCM_NATS_URL}"
fi
if [[ "${ENABLE_CONTAINER_SP}" == "true" ]]; then
    export DCM_CONTAINER_SP_URL="${DCM_CONTAINER_SP_URL:-http://localhost:8082/api/v1alpha1}"
    info "DCM_CONTAINER_SP_URL=${DCM_CONTAINER_SP_URL}"
fi
if [[ "${ENABLE_ACM_CLUSTER_SP}" == "true" ]]; then
    export DCM_ACM_CLUSTER_SP_URL="${DCM_ACM_CLUSTER_SP_URL:-http://localhost:8083/api/v1alpha1}"
    info "DCM_ACM_CLUSTER_SP_URL=${DCM_ACM_CLUSTER_SP_URL}"
fi
if [[ "${ENABLE_KUBEVIRT_SP}" == "true" ]]; then
    # Do not default standalone KubeVirt to :8081 when the agent owns that port.
    if [[ -n "${DCM_KUBEVIRT_SP_URL:-}" ]]; then
        export DCM_KUBEVIRT_SP_URL
        info "DCM_KUBEVIRT_SP_URL=${DCM_KUBEVIRT_SP_URL}"
    elif [[ "${WITH_ENVIRONMENT_AGENT}" == "true" && "${AGENT_PORT}" == "8081" ]]; then
        info "Standalone KubeVirt SP URL unset; agent owns :${AGENT_PORT} — Ginkgo will use embedded vm capability when present"
    else
        export DCM_KUBEVIRT_SP_URL="${DCM_KUBEVIRT_SP_URL:-http://localhost:8081/api/v1alpha1}"
        info "DCM_KUBEVIRT_SP_URL=${DCM_KUBEVIRT_SP_URL}"
    fi
    # Keep Ginkgo cluster lookups in the same NS the SP uses (compose KUBERNETES_NAMESPACE).
    if [[ -z "${KUBERNETES_NAMESPACE:-}" ]]; then
        export KUBERNETES_NAMESPACE="${KUBEVIRT_VM_NS_ARG:-${KUBEVIRT_VM_NAMESPACE:-vms}}"
    fi
    export KUBEVIRT_VM_NAMESPACE="${KUBEVIRT_VM_NAMESPACE:-${KUBERNETES_NAMESPACE}}"
    info "KUBERNETES_NAMESPACE=${KUBERNETES_NAMESPACE}"
fi
# Agent-embedded vm: keep Ginkgo lookups on the same NS written to SP_VM_NAMESPACE
# at deploy time (resolve_embedded_vm_namespace). Re-run for --skip-deploy paths.
if [[ "${WITH_ENVIRONMENT_AGENT}" == "true" ]] && agent_list_contains vm; then
    resolve_embedded_vm_namespace
fi

run_ginkgo_suite() {
    local label_filter="$1" report_file="$2" fail_on_empty="${3:-false}"
    local -a arguments=(-r -v --tags=e2e)
    if [[ -n "${label_filter}" ]]; then
        arguments+=(--label-filter="${label_filter}")
    fi
    if [[ -n "${report_file}" ]]; then
        arguments+=(--junit-report="${report_file}")
    fi
    if [[ "${fail_on_empty}" == true ]]; then
        arguments+=(--fail-on-empty)
    fi
    (cd "${TEST_DIR}" && go run github.com/onsi/ginkgo/v2/ginkgo "${arguments[@]}" .)
}

merge_junit_reports() {
    local regular_report="$1" disruptive_report="$2"

    python3 - "${regular_report}" "${disruptive_report}" <<'PY'
import sys
import xml.etree.ElementTree as ET

regular_path, disruptive_path = sys.argv[1:]
regular = ET.parse(regular_path)
disruptive = ET.parse(disruptive_path)
regular_root = regular.getroot()
disruptive_root = disruptive.getroot()

if regular_root.tag == "testsuite":
    suite = regular_root
    regular_root = ET.Element("testsuites")
    regular_root.append(suite)
if disruptive_root.tag == "testsuite":
    disruptive_suites = [disruptive_root]
else:
    disruptive_suites = list(disruptive_root.findall("testsuite"))

for suite in disruptive_suites:
    regular_root.append(suite)

tests = failures = errors = skipped = 0
total_time = 0.0
for suite in regular_root.findall("testsuite"):
    tests += int(suite.get("tests", 0))
    failures += int(suite.get("failures", 0))
    errors += int(suite.get("errors", 0))
    skipped += int(suite.get("skipped", 0))
    total_time += float(suite.get("time", 0))

regular_root.set("tests", str(tests))
regular_root.set("failures", str(failures))
regular_root.set("errors", str(errors))
regular_root.set("skipped", str(skipped))
regular_root.set("time", str(total_time))
ET.ElementTree(regular_root).write(regular_path, encoding="utf-8", xml_declaration=True)
PY
}

# Run the regular suite first. Disruptive authenticated tests run as a final
# ordered phase so RHBK restart and signing-key rotation cannot disrupt other specs.
log "Running E2E tests"
TEST_EXIT=0
REGULAR_FILTER="${LABEL_FILTER}"
if [[ "${AUTH_ENABLED}" == true ]]; then
    AUTH_DISRUPTIVE_FILTER='!(auth && disruptive)'
    if [[ -n "${REGULAR_FILTER}" ]]; then
        REGULAR_FILTER="(${REGULAR_FILTER}) && ${AUTH_DISRUPTIVE_FILTER}"
    else
        REGULAR_FILTER="${AUTH_DISRUPTIVE_FILTER}"
    fi
fi
run_ginkgo_suite "${REGULAR_FILTER}" "${JUNIT_REPORT}" || TEST_EXIT=$?

if [[ "${AUTH_DISRUPTIVE}" == true ]]; then
    log "Running disruptive authentication tests"
    DISRUPTIVE_EXIT=0
    DISRUPTIVE_REPORT=""
    if [[ -n "${JUNIT_REPORT}" ]]; then
        DISRUPTIVE_REPORT="${JUNIT_REPORT}.auth-disruptive.tmp"
    fi
    # Refresh credentials after the regular suite. TC-42 restarts RHBK and
    # TC-45 changes signing keys, so the destructive phase must not reuse an
    # expiring admin token created at the beginning of a long run.
    prepare_auth_test_settings || DISRUPTIVE_EXIT=$?
    if [[ "${DISRUPTIVE_EXIT}" -eq 0 ]]; then
        DISRUPTIVE_FILTER='auth && disruptive'
        if [[ -n "${LABEL_FILTER}" ]]; then
            DISRUPTIVE_FILTER="(${LABEL_FILTER}) && (${DISRUPTIVE_FILTER})"
        fi
        FAIL_ON_EMPTY=false
        if [[ -z "${LABEL_FILTER}" ]]; then
            FAIL_ON_EMPTY=true
        fi
        run_ginkgo_suite "${DISRUPTIVE_FILTER}" "${DISRUPTIVE_REPORT}" "${FAIL_ON_EMPTY}" || DISRUPTIVE_EXIT=$?
        if [[ -n "${JUNIT_REPORT}" && -f "${JUNIT_REPORT}" && -f "${DISRUPTIVE_REPORT}" ]]; then
            merge_junit_reports "${JUNIT_REPORT}" "${DISRUPTIVE_REPORT}"
            rm -f "${DISRUPTIVE_REPORT}"
        fi
    fi
    if [[ "${DISRUPTIVE_EXIT}" -ne 0 ]]; then
        TEST_EXIT="${DISRUPTIVE_EXIT}"
    fi
fi

if [[ "${TEST_EXIT}" -eq 0 ]]; then
    log "Tests passed"
else
    err "Tests failed (exit code: ${TEST_EXIT})"
fi

# Teardown the stack.
if [[ "${SKIP_TEARDOWN}" == "false" ]]; then
    log "Tearing down DCM stack"
    if ! "${DEPLOY_SCRIPT}" --tear-down "${DEPLOY_ARGS[@]+"${DEPLOY_ARGS[@]}"}"; then
        err "Teardown failed (non-fatal) — containers may still be running"
        err "Manual cleanup: ${DEPLOY_SCRIPT} --tear-down ${DEPLOY_ARGS[*]}"
    fi
else
    log "Skipping teardown (--skip-teardown)"
fi

exit "${TEST_EXIT}"
