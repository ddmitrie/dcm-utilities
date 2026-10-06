#!/usr/bin/env bash
set -euo pipefail

# DCM E2E Deploy Script
# Clones the control-plane repo, brings up the full DCM stack via podman-compose,
# and verifies all services are healthy.
#
# Service providers are configured via providers/*.conf files. To add a new
# provider, drop a .conf file in the providers/ directory — no changes to
# this script are required.

readonly DEFAULT_CONTROL_PLANE_REPO="https://github.com/dcm-project/control-plane.git"
readonly DEFAULT_CONTROL_PLANE_BRANCH="main"
readonly DEFAULT_CONTROL_PLANE_TMP_DIR="/tmp/dcm-e2e"
export COMPOSE_PROJECT_NAME="dcm-e2e"
# Use a Compose network with service-name DNS. Podman Compose pod mode can
# isolate services from each other when the pod has no shared network namespace.
export PODMAN_COMPOSE_IN_POD="${PODMAN_COMPOSE_IN_POD:-false}"
readonly CONTROL_PLANE_PORT="8080"
readonly HEALTH_TIMEOUT_SECONDS=90
readonly HEALTH_POLL_INTERVAL=5

readonly HEALTH_ENDPOINTS=(
    "/api/v1alpha1/health"
)

readonly DEFAULT_ACM_CLUSTER_SP_REPO="https://github.com/dcm-project/acm-cluster-service-provider.git"
readonly DEFAULT_ACM_CLUSTER_SP_BRANCH="main"

podman_compose() {
    command podman-compose --in-pod "${PODMAN_COMPOSE_IN_POD}" "$@"
}

readonly QUAY_VERSION_REPO="control-plane"
readonly VERSION_ENV_VARS=(
    CONTROL_PLANE_VERSION
    DCM_GITOPS_VERSION
    DCM_UI_VERSION
    ENVIRONMENT_AGENT_VERSION
    KUBEVIRT_SERVICE_PROVIDER_VERSION
    K8S_CONTAINER_SERVICE_PROVIDER_VERSION
    K8S_STORAGE_SERVICE_PROVIDER_VERSION
    K8S_NETWORK_SERVICE_PROVIDER_VERSION
    ACM_CLUSTER_SERVICE_PROVIDER_VERSION
    THREE_TIER_DEMO_SERVICE_PROVIDER_VERSION
)
readonly DEFAULT_AGENT_PORT="8081"  # same host port as standalone kubevirt SP (compose-kubevirt-sp.yaml); agent + kubevirt requires a distinct --agent-port
readonly AGENT_HEALTH_ENDPOINTS=(
    "/api/v1alpha1/health"
)

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly GITOPS_COMPOSE_OVERRIDE="${REPO_ROOT}/tests/compose-gitops.yaml"

# --- Provider registry ----------------------------------------------------- #
#
# Each providers/*.conf file defines a service provider. The registry loads
# them into parallel arrays indexed by provider number. All per-provider logic
# (arg parsing, validation, compose args, env exports) uses these arrays.

PROV_COUNT=0
PROV_LABELS=()
PROV_FLAGS=()
PROV_DESCRIPTIONS=()
PROV_PROFILES=()
PROV_OVERRIDES=()
PROV_CLI_REQS=()
PROV_NS_FLAGS=()
PROV_NS_ENVS=()
PROV_NS_DEFAULTS=()
PROV_KC_EXPORTS=()
PROV_NS_EXPORTS=()
PROV_VALIDATES=()
# Mutable state per provider (set during arg parsing / processing)
PROV_ENABLED=()
PROV_NAMESPACES=()
PROV_CLIS=()

load_providers() {
    local conf
    for conf in "${REPO_ROOT}/providers/"*.conf; do
        [[ -f "${conf}" ]] || continue

        # Source into a clean set of variables
        local PROVIDER_LABEL="" PROVIDER_FLAG="" PROVIDER_DESCRIPTION=""
        local COMPOSE_PROFILE="" COMPOSE_OVERRIDE="" CLI_REQUIREMENT=""
        local NAMESPACE_FLAG="" NAMESPACE_ENV="" NAMESPACE_DEFAULT=""
        local KUBECONFIG_EXPORT="" NAMESPACE_EXPORT="" VALIDATE_HOOK=""

        # shellcheck source=/dev/null
        source "${conf}"

        local i="${PROV_COUNT}"
        PROV_LABELS[i]="${PROVIDER_LABEL}"
        PROV_FLAGS[i]="${PROVIDER_FLAG}"
        PROV_DESCRIPTIONS[i]="${PROVIDER_DESCRIPTION}"
        PROV_PROFILES[i]="${COMPOSE_PROFILE}"
        PROV_OVERRIDES[i]="${COMPOSE_OVERRIDE}"
        PROV_CLI_REQS[i]="${CLI_REQUIREMENT}"
        PROV_NS_FLAGS[i]="${NAMESPACE_FLAG}"
        PROV_NS_ENVS[i]="${NAMESPACE_ENV}"
        PROV_NS_DEFAULTS[i]="${NAMESPACE_DEFAULT}"
        PROV_KC_EXPORTS[i]="${KUBECONFIG_EXPORT}"
        PROV_NS_EXPORTS[i]="${NAMESPACE_EXPORT}"
        PROV_VALIDATES[i]="${VALIDATE_HOOK}"

        # Initialize mutable state
        PROV_ENABLED[i]=false
        # Resolve default namespace from env var or default value
        local ns_env_val="${!NAMESPACE_ENV:-}"
        PROV_NAMESPACES[i]="${ns_env_val:-${NAMESPACE_DEFAULT}}"
        PROV_CLIS[i]=""

        PROV_COUNT=$((PROV_COUNT + 1))
    done
}

load_providers

# --- Usage ----------------------------------------------------------------- #

usage() {
    cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Deploy the full DCM stack for E2E testing. The control-plane repo contains
deploy/compose.yaml, which orchestrates the monolith, UI, infra, and providers.

Options:
  --version TAG                  Pin all DCM images to TAG (main, release, or explicit e.g. v0.1.0-rc.1)
  --control-plane-repo URL       Git repo for control-plane (default: ${DEFAULT_CONTROL_PLANE_REPO})
  --control-plane-branch REF     Branch to clone (default: ${DEFAULT_CONTROL_PLANE_BRANCH})
  --control-plane-dir PATH       Directory to clone control-plane into (default: ${DEFAULT_CONTROL_PLANE_TMP_DIR})
  --all-service-providers        Enable all available service providers
  --gitops                       Enable the dcm-gitops reconciliation container
  --with-environment-agent       Enable the environment-agent compose profile (embedded SPs)
  --agent-embedded-sps LIST      Comma-separated embedded SPs (required with agent; e.g. container,vm)
  --agent-port PORT              Host port for environment-agent API (default: ${DEFAULT_AGENT_PORT})
EOF

    # Provider flags (generated from registry)
    local i
    for i in $(seq 0 $((PROV_COUNT - 1))); do
        printf "  --%-30s %s\n" "${PROV_FLAGS[$i]}" "${PROV_DESCRIPTIONS[$i]}"
    done

    cat <<EOF
  --deploy-acm                   Deploy ACM on the cluster before starting the stack (opt-in, heavy)
  --deploy-mce                   Deploy MCE on the cluster before starting the stack (opt-in, heavy)
  --deploy-cnv                   Deploy OpenShift Virtualization (CNV) on the cluster before starting the stack (opt-in, heavy)
  --acm-cluster-sp-repo URL      Git repo for acm-cluster-service-provider (default: ${DEFAULT_ACM_CLUSTER_SP_REPO})
  --acm-cluster-sp-branch REF    Branch to clone (default: ${DEFAULT_ACM_CLUSTER_SP_BRANCH})
  --kubeconfig PATH              Path to kubeconfig file (auto-detected if omitted; mounted into the agent)
EOF

    # Namespace flags (generated from registry)
    for i in $(seq 0 $((PROV_COUNT - 1))); do
        printf "  --%-30s Namespace for %s (default: %s)\n" \
            "${PROV_NS_FLAGS[$i]} NS" "${PROV_LABELS[$i]}" "${PROV_NS_DEFAULTS[$i]}"
    done

    cat <<EOF
  --cluster-api URL              OpenShift API URL for oc login
  --cluster-username USER        Username for oc login (default: kubeadmin)
  --cluster-password PASS        Password for oc login
  --compose-file PATH            Additional compose file to merge (repeatable, e.g. port overrides)
  --auth-enabled                 Enable authentication (loads auth Compose override and profile; starts Keycloak)
  --cleanup-on-failure           Tear down the stack automatically if deployment fails (default: leave for debugging)
  --running-versions             Print versions of all running containers and write dcm-versions.json
  --tear-down                    Stop the stack, remove volumes, and clean the deploy directory
  --help                         Show this help message

Cluster authentication (when any service provider is enabled):
  The script resolves cluster credentials in this order:
    1. Explicit --kubeconfig PATH (or KUBECONFIG env var)
    2. Existing oc/kubectl session (oc whoami or kubectl cluster-info)
    3. oc login with --cluster-api + --cluster-password

Environment variables (flags take precedence):
  DCM_VERSION               Same as --version
  DCM_GITOPS_VERSION        Image tag for dcm-gitops (default: main; --version also pins it)
  CONTROL_PLANE_REPO        Same as --control-plane-repo
  CONTROL_PLANE_BRANCH      Same as --control-plane-branch
  CONTROL_PLANE_TMP_DIR     Same as --control-plane-dir
  KUBECONFIG                Same as --kubeconfig
  OPENSHIFT_API             Same as --cluster-api
  OPENSHIFT_USERNAME        Same as --cluster-username (default: kubeadmin)
  OPENSHIFT_PASSWORD        Same as --cluster-password
  AUTH_DISABLED             Set to 'false' to enable auth (same effect as --auth-enabled)
  PODMAN_COMPOSE_IN_POD     Podman Compose pod mode (default: false)
EOF

    # Provider namespace env vars (generated from registry)
    for i in $(seq 0 $((PROV_COUNT - 1))); do
        printf "  %-25s Same as --%s (default: %s)\n" \
            "${PROV_NS_ENVS[$i]}" "${PROV_NS_FLAGS[$i]}" "${PROV_NS_DEFAULTS[$i]}"
    done

    cat <<EOF
  ACM_CHANNEL               Override ACM subscription channel (auto-detect)
  MCE_CHANNEL               Override MCE subscription channel (auto-detect)
  CSV_TIMEOUT               Seconds to wait for operator CSV (default: 300)
  DEPLOY_TIMEOUT            Seconds to wait for ACM/MCE CR readiness (default: 1200)

Examples:
  $(basename "$0")
  $(basename "$0") --version v0.1.0-rc.1
  $(basename "$0") --version release
  $(basename "$0") --control-plane-branch feature-x
  $(basename "$0") --kubevirt-service-provider --kubeconfig ~/.kube/config
  $(basename "$0") --k8s-container-service-provider
  $(basename "$0") --k8s-storage-service-provider --kubeconfig ~/.kube/config
  $(basename "$0") --all-service-providers --cluster-api https://api.cluster.example.com --cluster-password secret
  $(basename "$0") --acm-cluster-service-provider --deploy-acm --kubeconfig ~/.kube/config
  $(basename "$0") --auth-enabled
  $(basename "$0") --tear-down
  $(basename "$0") --running-versions
EOF
}

# --- Logging --------------------------------------------------------------- #

log()  { echo "==> $*"; }
info() { echo "    $*"; }
err()  { echo "ERROR: $*" >&2; }

# --- Prerequisite helpers -------------------------------------------------- #

validate_deploy_dir() {
    local dir="$1"

    case "${dir}" in
        /|/bin|/boot|/dev|/etc|/home|/lib*|/opt|/proc|/root|/run|/sbin|/srv|/sys|/tmp|/usr|/var)
            err "Refusing to use system path as deploy directory: ${dir}"
            return 1 ;;
    esac

    if [[ "${dir}" == "/" ]] || [[ -z "${dir}" ]]; then
        err "Deploy directory path is empty or root — aborting"
        return 1
    fi
}

check_required_tools() {
    local missing=()
    for tool in "$@"; do
        if ! command -v "${tool}" &>/dev/null; then
            missing+=("${tool}")
        fi
    done

    if [[ ${#missing[@]} -gt 0 ]]; then
        err "Missing required tools: ${missing[*]}"
        err "Install them before running this script."
        return 1
    fi
}

ensure_podman_running() {
    if podman info &>/dev/null; then
        return 0
    fi

    # On macOS, Podman runs inside a VM that must be started explicitly
    if podman machine list --format '{{.Name}}' &>/dev/null; then
        info "Podman machine is not running — starting it..."
        local output
        if output=$(podman machine start 2>&1); then
            info "Podman machine started"
            return 0
        fi
        err "Failed to start Podman machine: ${output}"
        err "Try manually: podman machine start"
        return 1
    fi

    err "Podman daemon is not reachable and no Podman machine found"
    err "Install or start Podman before running this script"
    return 1
}

# --- Tear-down ------------------------------------------------------------- #

tear_down() {
    local deploy_dir="$1"
    shift
    local compose_profiles=("$@")

    log "Tearing down DCM stack"

    if [[ -d "${deploy_dir}" ]]; then
        info "Stopping containers and removing volumes..."
        podman_compose -f "${deploy_dir}/deploy/compose.yaml" ${compose_profiles[@]+"${compose_profiles[@]}"} down -v 2>/dev/null || true

        local project_name="${COMPOSE_PROJECT_NAME}"
        local remaining
        remaining=$(podman ps -a --filter "name=${project_name}_" --format '{{.ID}}' 2>/dev/null || true)
        if [[ -n "${remaining}" ]]; then
            info "Force-removing remaining containers..."
            echo "${remaining}" | xargs -r podman rm -f 2>/dev/null || true
        fi

        podman pod ls --filter "name=${project_name}" --format '{{.ID}}' 2>/dev/null | xargs -r podman pod rm -f 2>/dev/null || true
        podman network rm -f "${project_name}_default" 2>/dev/null || true

        local stale_volumes
        stale_volumes=$(podman volume ls --format '{{.Name}}' 2>/dev/null | grep "^${project_name}_" || true)
        if [[ -n "${stale_volumes}" ]]; then
            info "Removing named volumes..."
            echo "${stale_volumes}" | xargs -r podman volume rm 2>/dev/null || true
        fi

        info "Removing deploy directory: ${deploy_dir}"
        rm -rf "${deploy_dir}"
    fi

    log "Tear-down complete"
}

# --- Provider validation hooks -------------------------------------------- #
#
# Each hook receives: (kubeconfig, namespace, cli_binary)
# Hooks use what they need and ignore the rest.

validate_kubevirt_provider() {
    local kubeconfig="$1"
    local namespace="$2"

    log "Validating kubevirt provider prerequisites"

    info "Checking for OpenShift Virtualization (kubevirt.io CRDs)..."
    if ! oc --kubeconfig="${kubeconfig}" get crd virtualmachines.kubevirt.io &>/dev/null; then
        err "kubevirt.io CRDs not found — OpenShift Virtualization (CNV) must be installed"
        return 1
    fi
    info "OpenShift Virtualization is installed"

    info "Ensuring namespace '${namespace}' exists..."
    if ! oc --kubeconfig="${kubeconfig}" get namespace "${namespace}" &>/dev/null; then
        info "Creating namespace '${namespace}'..."
        oc --kubeconfig="${kubeconfig}" create namespace "${namespace}"
    fi
    info "Namespace '${namespace}' is ready"
}

ensure_provider_namespace() {
    local kubeconfig="$1"
    local namespace="$2"
    local cli="$3"

    info "Ensuring namespace '${namespace}' exists..."
    if ! "${cli}" --kubeconfig="${kubeconfig}" get namespace "${namespace}" &>/dev/null; then
        info "Creating namespace '${namespace}'..."
        "${cli}" --kubeconfig="${kubeconfig}" create namespace "${namespace}"
    fi
    info "Namespace '${namespace}' is ready"
}

validate_k8s_container_provider() {
    log "Validating k8s container provider prerequisites"
    ensure_provider_namespace "$1" "$2" "$3"
}

validate_k8s_storage_provider() {
    log "Validating k8s storage provider prerequisites"
    ensure_provider_namespace "$1" "$2" "$3"
}

validate_acm_cluster_provider() {
    local kubeconfig="$1"
    local namespace="$2"

    log "Validating ACM cluster provider prerequisites"

    ensure_provider_namespace "${kubeconfig}" "${namespace}" oc

    resolve_cluster_pull_secret "${kubeconfig}" || return 1
}

# Resolve pull secret for ACM / embedded cluster SP.
# Prefer ACM_CLUSTER_SP_PULL_SECRET or SP_PULL_SECRET if already set; otherwise
# extract base64 .dockerconfigjson from openshift-config/pull-secret.
resolve_cluster_pull_secret() {
    local kubeconfig="$1"

    if [[ -n "${SP_PULL_SECRET:-}" ]]; then
        export ACM_CLUSTER_SP_PULL_SECRET="${ACM_CLUSTER_SP_PULL_SECRET:-${SP_PULL_SECRET}}"
        return 0
    fi
    if [[ -n "${ACM_CLUSTER_SP_PULL_SECRET:-}" ]]; then
        export SP_PULL_SECRET="${SP_PULL_SECRET:-${ACM_CLUSTER_SP_PULL_SECRET}}"
        return 0
    fi

    info "Resolving pull secret from cluster..."
    local pull_json
    pull_json=$(oc --kubeconfig="${kubeconfig}" get secret pull-secret \
        -n openshift-config -o jsonpath='{.data.\.dockerconfigjson}' 2>/dev/null || echo "")
    if [[ -n "${pull_json}" ]]; then
        export ACM_CLUSTER_SP_PULL_SECRET="${pull_json}"
        export SP_PULL_SECRET="${pull_json}"
        info "Pull secret resolved from openshift-config/pull-secret"
        return 0
    fi

    err "Could not resolve cluster pull secret (openshift-config/pull-secret)"
    err "Set SP_PULL_SECRET or ACM_CLUSTER_SP_PULL_SECRET to a base64-encoded .dockerconfigjson"
    return 1
}

# Prepare required env for embedded cluster SP (fail-fast before compose up).
prepare_embedded_cluster_sp() {
    local kubeconfig="$1"

    SP_CLUSTER_NAMESPACE="${SP_CLUSTER_NAMESPACE:-clusters}"
    export SP_CLUSTER_NAMESPACE
    info "Embedded cluster SP namespace: ${SP_CLUSTER_NAMESPACE}"
    ensure_provider_namespace "${kubeconfig}" "${SP_CLUSTER_NAMESPACE}" oc || return 1

    resolve_cluster_pull_secret "${kubeconfig}" || return 1
    if [[ -z "${SP_PULL_SECRET:-}" ]]; then
        err "SP_PULL_SECRET is required when embedding the cluster SP"
        return 1
    fi
}

# Return 0 if AGENT_EMBEDDED_SPS contains the given service type token.
agent_embeds() {
    local want="$1"
    local tok
    IFS=',' read -r -a _agent_embed_toks <<< "${AGENT_EMBEDDED_SPS}"
    for tok in "${_agent_embed_toks[@]}"; do
        tok="${tok// /}"
        [[ "${tok}" == "${want}" ]] && return 0
    done
    return 1
}

# --- Cluster authentication ------------------------------------------------ #

resolve_kubeconfig() {
    if [[ -n "${DCM_KUBECONFIG}" ]]; then
        if [[ ! -f "${DCM_KUBECONFIG}" ]]; then
            err "Kubeconfig file not found: ${DCM_KUBECONFIG}"
            return 1
        fi
        info "Using kubeconfig: ${DCM_KUBECONFIG}"
        return 0
    fi

    if command -v oc &>/dev/null && oc whoami &>/dev/null; then
        DCM_KUBECONFIG="${HOME}/.kube/config"
        info "Using existing oc session ($(oc whoami))"
        return 0
    elif command -v kubectl &>/dev/null && kubectl cluster-info &>/dev/null 2>&1; then
        DCM_KUBECONFIG="${HOME}/.kube/config"
        info "Using existing kubectl context"
        return 0
    fi

    if [[ -n "${OPENSHIFT_API:-}" ]] && [[ -n "${OPENSHIFT_PASSWORD:-}" ]]; then
        if ! command -v oc &>/dev/null; then
            err "'oc' is required for --cluster-api login"
            return 1
        fi
        info "Logging in to ${OPENSHIFT_API}..."
        oc login "${OPENSHIFT_API}" \
            --username="${OPENSHIFT_USERNAME:-kubeadmin}" \
            --password="${OPENSHIFT_PASSWORD}"
        DCM_KUBECONFIG="${HOME}/.kube/config"
        info "Logged in as $(oc whoami)"
        return 0
    fi

    err "No cluster credentials found. Provide --kubeconfig, set KUBECONFIG,"
    err "log in with 'oc login', or set OPENSHIFT_API + OPENSHIFT_PASSWORD."
    return 1
}

# --- Health verification --------------------------------------------------- #

verify_health() {
    local compose_file="$1"
    shift
    local compose_profiles=("$@")

    log "Verifying service health"

    info "Checking container readiness..."
    local expected_services running_services
    expected_services=$(podman_compose -f "${compose_file}" ${compose_profiles[@]+"${compose_profiles[@]}"} config --services 2>/dev/null | sort)
    running_services=$(podman_compose -f "${compose_file}" ${compose_profiles[@]+"${compose_profiles[@]}"} ps 2>/dev/null | awk 'NR>1 {print $NF}' | sed 's/.*_\(.*\)_[0-9]*/\1/' | sort)

    local container_failures=()
    while IFS= read -r service; do
        [[ -z "${service}" ]] && continue
        if ! echo "${running_services}" | grep -qx "${service}"; then
            container_failures+=("${service}")
        fi
    done <<< "${expected_services}"

    if [[ ${#container_failures[@]} -gt 0 ]]; then
        err "The following services are not running: ${container_failures[*]}"
        err "Check logs with: podman-compose -f ${compose_file} logs <service>"
        return 1
    fi
    info "All containers running"

    info "Polling health endpoints (timeout: ${HEALTH_TIMEOUT_SECONDS}s)..."

    local control_plane_url="http://localhost:${CONTROL_PLANE_PORT}"
    local health_failures=()

    for endpoint in "${HEALTH_ENDPOINTS[@]}"; do
        local healthy=false
        local attempt_elapsed=0

        while [[ ${attempt_elapsed} -lt ${HEALTH_TIMEOUT_SECONDS} ]]; do
            local http_code
            http_code=$(curl -s --connect-timeout 5 --max-time 10 -o /dev/null -w "%{http_code}" "${control_plane_url}${endpoint}" 2>/dev/null || echo "000")
            if [[ "${http_code}" =~ ^2[0-9]{2}$ ]]; then
                healthy=true
                break
            fi
            sleep "${HEALTH_POLL_INTERVAL}"
            attempt_elapsed=$((attempt_elapsed + HEALTH_POLL_INTERVAL))
        done

        if [[ "${healthy}" == true ]]; then
            info "  PASS  ${endpoint}"
        else
            info "  FAIL  ${endpoint} (last HTTP ${http_code})"
            health_failures+=("${endpoint}")
        fi
    done

    if [[ "${WITH_ENVIRONMENT_AGENT}" == true ]]; then
        local agent_url="http://localhost:${AGENT_PORT}"
        info "Polling environment-agent health endpoints (timeout: ${HEALTH_TIMEOUT_SECONDS}s)..."
        for endpoint in "${AGENT_HEALTH_ENDPOINTS[@]}"; do
            local healthy=false
            local attempt_elapsed=0
            local http_code="000"

            while [[ "${attempt_elapsed}" -lt "${HEALTH_TIMEOUT_SECONDS}" ]]; do
                http_code=$(curl -s --connect-timeout 5 --max-time 10 -o /dev/null -w "%{http_code}" "${agent_url}${endpoint}" 2>/dev/null || echo "000")
                if [[ "${http_code}" =~ ^2[0-9]{2}$ ]]; then
                    healthy=true
                    break
                fi
                sleep "${HEALTH_POLL_INTERVAL}"
                attempt_elapsed=$((attempt_elapsed + HEALTH_POLL_INTERVAL))
            done

            if [[ "${healthy}" == true ]]; then
                info "  PASS  agent ${endpoint}"
            else
                info "  FAIL  agent ${endpoint} (last HTTP ${http_code})"
                health_failures+=("agent:${endpoint}")
            fi
        done
    fi

    echo
    if [[ ${#health_failures[@]} -gt 0 ]]; then
        err "Health check failed for: ${health_failures[*]}"
        err "Check logs with: podman-compose -f ${compose_file} logs"
        return 1
    fi
}

# --- Version resolution ---------------------------------------------------- #

resolve_latest_version() {
    local api_url="https://quay.io/api/v1/repository/dcm-project/${QUAY_VERSION_REPO}/tag/?onlyActiveTags=true&limit=100&filter_tag_name=like:v%"
    local api_response
    api_response=$(curl -s --connect-timeout 5 --max-time 10 "${api_url}" 2>/dev/null || echo "")

    if [[ -z "${api_response}" ]]; then
        err "Quay API unreachable — cannot resolve latest version"
        return 1
    fi

    local latest
    latest=$(echo "${api_response}" | jq -r '.tags[].name' 2>/dev/null | grep -E '^v[0-9]' | sort -V | tail -1)

    if [[ -z "${latest}" ]]; then
        err "No semver tags found in quay.io/dcm-project/${QUAY_VERSION_REPO}"
        return 1
    fi

    echo "${latest}"
}

# --- Running versions ------------------------------------------------------ #

resolve_git_sha() {
    local repo_name="$1"
    local image_digest="$2"

    local api_url="https://quay.io/api/v1/repository/dcm-project/${repo_name}/tag/?onlyActiveTags=true&limit=100"
    local api_response
    api_response=$(curl -s --connect-timeout 5 --max-time 10 "${api_url}" 2>/dev/null || echo "")

    if [[ -z "${api_response}" ]]; then
        info "  WARN  Quay API unreachable for ${repo_name}"
        return 1
    fi

    local matched_sha
    matched_sha=$(echo "${api_response}" | jq -r --arg digest "${image_digest}" '.tags[] | select(.manifest_digest == $digest) | .name' 2>/dev/null | grep -E '^sha-[a-f0-9]+$' | head -1)

    if [[ -z "${matched_sha}" ]]; then
        info "  WARN  Could not resolve git SHA for ${repo_name} (digest: ${image_digest:0:19}...)"
        return 1
    fi

    echo "${matched_sha#sha-}"
}

get_running_versions() {
    local compose_file="$1"
    shift
    local compose_profiles=("$@")

    if [[ ! -f "${compose_file}" ]]; then
        err "Compose file not found: ${compose_file}"
        err "Is the DCM stack deployed? Use --control-plane-dir to specify the deploy directory."
        return 1
    fi

    log "Collecting running container versions"

    local container_ids
    container_ids=$(podman_compose -f "${compose_file}" ${compose_profiles[@]+"${compose_profiles[@]}"} ps -q 2>/dev/null)

    if [[ -z "${container_ids}" ]]; then
        err "No running containers found"
        return 1
    fi

    local entries=()

    while IFS= read -r container_id; do
        [[ -z "${container_id}" ]] && continue

        local image_name image_digest
        read -r image_name image_digest < <(podman inspect --format '{{.ImageName}} {{.ImageDigest}}' "${container_id}" 2>/dev/null || echo "unknown unknown")

        local git_sha="null"
        if [[ "${image_name}" == quay.io/dcm-project/* ]]; then
            local repo_name resolved_sha
            repo_name="${image_name#quay.io/dcm-project/}"
            repo_name="${repo_name%%:*}"

            if resolved_sha=$(resolve_git_sha "${repo_name}" "${image_digest}"); then
                git_sha="\"${resolved_sha}\""
            fi
        fi

        entries+=("$(jq -n --arg image "${image_name}" --arg digest "${image_digest}" --argjson git_sha "${git_sha}" '{($image): {image_digest: $digest, git_sha: $git_sha}}')")
    done <<< "${container_ids}"

    local output_file="${PWD}/dcm-versions.json"

    echo
    log "Container versions"
    printf '%s\n' "${entries[@]}" | jq -s 'add' | tee "${output_file}"
    echo
    info "Versions written to ${output_file}"
}

# --- Provider helpers ------------------------------------------------------ #

# Resolve the CLI binary for a provider based on its CLI_REQUIREMENT.
# Sets PROV_CLIS[$1] and adds to REQUIRED_TOOLS if needed.
resolve_provider_cli() {
    local i="$1"
    local req="${PROV_CLI_REQS[$i]}"

    case "${req}" in
        oc)
            PROV_CLIS[i]="oc"
            REQUIRED_TOOLS+=(oc)
            ;;
        oc-or-kubectl)
            if command -v oc &>/dev/null; then
                PROV_CLIS[i]="oc"
            elif command -v kubectl &>/dev/null; then
                PROV_CLIS[i]="kubectl"
            else
                REQUIRED_TOOLS+=(oc)
                PROV_CLIS[i]="oc"
            fi
            ;;
        *)
            PROV_CLIS[i]=""
            ;;
    esac
}

# --- Compose credential bootstrap ------------------------------------------ #
#
# control-plane deploy/compose.yaml reads credentials from deploy/.env
# (see control-plane deploy/.env.example). Bootstrap that file after clone.

env_or_default() {
    local var_name="$1"
    local default_value="$2"
    if [[ -n "${!var_name:-}" ]]; then
        echo "${!var_name}"
    else
        echo "${default_value}"
    fi
}

resolve_db_password() {
    local password_var
    local resolved_password=""
    local explicit_password=""

    for password_var in POSTGRES_PASSWORD DB_PASS DB_PASSWORD; do
        if [[ -n "${!password_var:-}" ]]; then
            if [[ -z "${explicit_password}" ]]; then
                explicit_password="${!password_var}"
                resolved_password="${password_var}=${!password_var}"
            elif [[ "${!password_var}" != "${explicit_password}" ]]; then
                err "Conflicting database passwords supplied via ${resolved_password%%=*} and ${password_var}"
                return 1
            fi
        fi
    done

    if [[ -z "${explicit_password}" ]]; then
        explicit_password="adminpass"
    fi

    printf '%s\n' "${explicit_password}"
}

upsert_deploy_env_var() {
    local deploy_dir="$1"
    local key="$2"
    local value="$3"
    local env_file="${deploy_dir}/deploy/.env"
    local tmp

    [[ -n "${value}" ]] || return 0

    tmp="$(mktemp)"
    if [[ -f "${env_file}" ]]; then
        grep -v "^${key}=" "${env_file}" > "${tmp}" || true
    fi
    printf '%s=%s\n' "${key}" "${value}" >> "${tmp}"
    mv "${tmp}" "${env_file}"
}

ensure_deploy_env() {
    local deploy_dir="$1"
    local env_file="${deploy_dir}/deploy/.env"
    local env_example="${deploy_dir}/deploy/.env.example"
    local var
    local db_password

    if [[ ! -f "${env_file}" ]]; then
        if [[ ! -f "${env_example}" ]]; then
            err "Missing ${env_example} — cannot bootstrap deploy credentials"
            return 1
        fi
        cp "${env_example}" "${env_file}"
        info "Created ${env_file} from .env.example"
    fi

    db_password="$(resolve_db_password)" || return 1
    upsert_deploy_env_var "${deploy_dir}" "POSTGRES_USER" "$(env_or_default POSTGRES_USER admin)"
    upsert_deploy_env_var "${deploy_dir}" "POSTGRES_PASSWORD" "${db_password}"
    upsert_deploy_env_var "${deploy_dir}" "DB_USER" "$(env_or_default DB_USER admin)"
    upsert_deploy_env_var "${deploy_dir}" "DB_PASS" "${db_password}"
    upsert_deploy_env_var "${deploy_dir}" "DB_PASSWORD" "${db_password}"

    if [[ "${AUTH_ENABLED}" == true ]]; then
        upsert_deploy_env_var "${deploy_dir}" "KEYCLOAK_ADMIN" "$(env_or_default KEYCLOAK_ADMIN admin)"
        upsert_deploy_env_var "${deploy_dir}" "KEYCLOAK_ADMIN_PASSWORD" "$(env_or_default KEYCLOAK_ADMIN_PASSWORD admin)"
        upsert_deploy_env_var "${deploy_dir}" "DCM_DEV_USER_PASSWORD" "$(env_or_default DCM_DEV_USER_PASSWORD admin)"
        upsert_deploy_env_var "${deploy_dir}" "AUTH_PROXY_SECRET" "$(env_or_default AUTH_PROXY_SECRET dcm-dev-proxy-secret)"
        upsert_deploy_env_var "${deploy_dir}" "AUTH_DISABLED" "false"
        upsert_deploy_env_var "${deploy_dir}" "AUTH_ISSUER_URL" "$(env_or_default AUTH_ISSUER_URL http://keycloak:8080/realms/dcm)"
        upsert_deploy_env_var "${deploy_dir}" "AUTH_JWT_AUDIENCE" "$(env_or_default AUTH_JWT_AUDIENCE dcm-api)"
        upsert_deploy_env_var "${deploy_dir}" "DCM_ADMIN_SUBJECT" "$(env_or_default DCM_ADMIN_SUBJECT 56deb662-4820-5d83-b828-f4beb11a5fa7)"
    else
        upsert_deploy_env_var "${deploy_dir}" "AUTH_DISABLED" "true"
    fi

    if [[ -n "${DCM_VERSION:-}" ]]; then
        for var in "${VERSION_ENV_VARS[@]}"; do
            upsert_deploy_env_var "${deploy_dir}" "${var}" "${DCM_VERSION}"
        done
    else
        for var in "${VERSION_ENV_VARS[@]}"; do
            if [[ -n "${!var:-}" ]]; then
                upsert_deploy_env_var "${deploy_dir}" "${var}" "${!var}"
            fi
        done
    fi

    if [[ -n "${ACM_CLUSTER_SP_PULL_SECRET:-}" ]]; then
        upsert_deploy_env_var "${deploy_dir}" "ACM_CLUSTER_SP_PULL_SECRET" "${ACM_CLUSTER_SP_PULL_SECRET}"
    fi

    if [[ "${WITH_ENVIRONMENT_AGENT}" == true ]]; then
        upsert_deploy_env_var "${deploy_dir}" "AGENT_EMBEDDED_SPS" "${AGENT_EMBEDDED_SPS}"
        upsert_deploy_env_var "${deploy_dir}" "AGENT_NAME" "$(env_or_default AGENT_NAME local-agent)"
        upsert_deploy_env_var "${deploy_dir}" "AGENT_ENVIRONMENT" "$(env_or_default AGENT_ENVIRONMENT dev)"
        upsert_deploy_env_var "${deploy_dir}" "AGENT_COST" "$(env_or_default AGENT_COST low)"
        upsert_deploy_env_var "${deploy_dir}" "AGENT_PORT" "${AGENT_PORT}"
        # Absolute host path for the compose bind mount (SP_DEFAULT_KUBECONFIG=/kubeconfig in compose).
        upsert_deploy_env_var "${deploy_dir}" "AGENT_KUBECONFIG_HOST" "${DCM_KUBECONFIG}"
        upsert_deploy_env_var "${deploy_dir}" "SP_CONTAINER_NAMESPACE" "$(env_or_default SP_CONTAINER_NAMESPACE default)"
        upsert_deploy_env_var "${deploy_dir}" "SP_K8S_EXTERNAL_SVC_TYPE" "$(env_or_default SP_K8S_EXTERNAL_SVC_TYPE NodePort)"
        # Prefer SP_VM_NAMESPACE; else KUBEVIRT_VM_NAMESPACE from --kubevirt-vm-namespace
        # / env (even when standalone kubevirt is not enabled). Default remains "default"
        # for the agent-embedded path (standalone kubevirt defaults to "vms").
        upsert_deploy_env_var "${deploy_dir}" "SP_VM_NAMESPACE" "$(env_or_default SP_VM_NAMESPACE "$(env_or_default KUBEVIRT_VM_NAMESPACE default)")"
        upsert_deploy_env_var "${deploy_dir}" "SP_STORAGE_NAMESPACE" "$(env_or_default SP_STORAGE_NAMESPACE default)"
        if agent_embeds cluster; then
            # Required by embedded acmcluster config — never leave empty placeholders
            # from .env.example (empty SP_PULL_SECRET causes agent exit 1).
            upsert_deploy_env_var "${deploy_dir}" "SP_CLUSTER_NAMESPACE" "${SP_CLUSTER_NAMESPACE:-clusters}"
            upsert_deploy_env_var "${deploy_dir}" "SP_PULL_SECRET" "${SP_PULL_SECRET:-${ACM_CLUSTER_SP_PULL_SECRET}}"
            if [[ -n "${SP_BASE_DOMAIN:-}" ]]; then
                upsert_deploy_env_var "${deploy_dir}" "SP_BASE_DOMAIN" "${SP_BASE_DOMAIN}"
            fi
        fi
    fi
}

# Collect compose args (profiles and overrides) for an enabled provider.
collect_provider_compose() {
    local i="$1"

    if [[ -n "${PROV_PROFILES[$i]}" ]]; then
        COMPOSE_PROFILES+=("--profile" "${PROV_PROFILES[$i]}")
    fi

    if [[ -n "${PROV_OVERRIDES[$i]}" ]]; then
        local override_path="${REPO_ROOT}/${PROV_OVERRIDES[$i]}"
        if [[ -f "${override_path}" ]]; then
            override_path="$(cd "$(dirname "${override_path}")" && pwd)/$(basename "${override_path}")"
            COMPOSE_EXTRA_FILE_ARGS+=("-f" "${override_path}")
            info "Injecting compose override for ${PROV_LABELS[$i]}: ${override_path}"
        else
            err "Compose override not found for ${PROV_LABELS[$i]}: ${override_path}"
            exit 1
        fi
    fi
}

# --- Argument parsing ------------------------------------------------------ #

DCM_VERSION="${DCM_VERSION:-}"
CONTROL_PLANE_REPO="${CONTROL_PLANE_REPO:-${DEFAULT_CONTROL_PLANE_REPO}}"
CONTROL_PLANE_BRANCH_EXPLICIT=false
[[ -n "${CONTROL_PLANE_BRANCH:-}" ]] && CONTROL_PLANE_BRANCH_EXPLICIT=true
CONTROL_PLANE_BRANCH="${CONTROL_PLANE_BRANCH:-${DEFAULT_CONTROL_PLANE_BRANCH}}"
CONTROL_PLANE_TMP_DIR="${CONTROL_PLANE_TMP_DIR:-${DEFAULT_CONTROL_PLANE_TMP_DIR}}"
TEAR_DOWN=false
RUNNING_VERSIONS=false
CLEANUP_ON_FAILURE=false
DEPLOY_ACM_MCE=""
DEPLOY_CNV=false
GITOPS_ENABLED=false
ACM_CLUSTER_SP_REPO="${DEFAULT_ACM_CLUSTER_SP_REPO}"
ACM_CLUSTER_SP_BRANCH="${DEFAULT_ACM_CLUSTER_SP_BRANCH}"
DCM_KUBECONFIG="${KUBECONFIG:-}"
OPENSHIFT_API="${OPENSHIFT_API:-}"
OPENSHIFT_USERNAME="${OPENSHIFT_USERNAME:-kubeadmin}"
OPENSHIFT_PASSWORD="${OPENSHIFT_PASSWORD:-}"
AUTH_ENABLED_EXPLICIT=false
COMPOSE_EXTRA_FILE_ARGS=()
WITH_ENVIRONMENT_AGENT=false
AGENT_EMBEDDED_SPS="${AGENT_EMBEDDED_SPS:-}"
AGENT_PORT="${AGENT_PORT:-${DEFAULT_AGENT_PORT}}"

require_arg() {
    if [[ -z "${2:-}" ]] || [[ "$2" == --* ]]; then
        err "Option $1 requires a value"
        usage; exit 1
    fi
}

# Match a flag against loaded provider flags/namespace flags.
# Returns 0 and sets MATCHED_IDX if found, returns 1 otherwise.
match_provider_flag() {
    local flag="$1"
    local i
    for i in $(seq 0 $((PROV_COUNT - 1))); do
        if [[ "${flag}" == "--${PROV_FLAGS[$i]}" ]]; then
            MATCHED_IDX="${i}"
            MATCHED_TYPE="enable"
            return 0
        fi
        if [[ "${flag}" == "--${PROV_NS_FLAGS[$i]}" ]]; then
            MATCHED_IDX="${i}"
            MATCHED_TYPE="namespace"
            return 0
        fi
    done
    return 1
}

MATCHED_IDX=""
MATCHED_TYPE=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --version)
            require_arg "$1" "${2:-}"
            DCM_VERSION="${2:-}"; shift 2 ;;
        --control-plane-repo)
            require_arg "$1" "${2:-}"
            CONTROL_PLANE_REPO="${2:-}"; shift 2 ;;
        --control-plane-branch)
            require_arg "$1" "${2:-}"
            CONTROL_PLANE_BRANCH="${2:-}"
            CONTROL_PLANE_BRANCH_EXPLICIT=true; shift 2 ;;
        --control-plane-dir)
            require_arg "$1" "${2:-}"
            CONTROL_PLANE_TMP_DIR="${2:-}"; shift 2 ;;
        --all-service-providers)
            for i in $(seq 0 $((PROV_COUNT - 1))); do
                PROV_ENABLED[i]=true
            done
            shift ;;
        --gitops)
            GITOPS_ENABLED=true; shift ;;
        --with-environment-agent)
            WITH_ENVIRONMENT_AGENT=true; shift ;;
        --agent-embedded-sps)
            require_arg "$1" "${2:-}"
            AGENT_EMBEDDED_SPS="${2:-}"; shift 2 ;;
        --agent-port)
            require_arg "$1" "${2:-}"
            AGENT_PORT="${2:-}"; shift 2 ;;
        --deploy-cnv)
            DEPLOY_CNV=true; shift ;;
        --deploy-acm)
            [[ -n "${DEPLOY_ACM_MCE}" ]] && { err "--deploy-acm and --deploy-mce are mutually exclusive"; exit 1; }
            DEPLOY_ACM_MCE="acm"; shift ;;
        --deploy-mce)
            [[ -n "${DEPLOY_ACM_MCE}" ]] && { err "--deploy-acm and --deploy-mce are mutually exclusive"; exit 1; }
            DEPLOY_ACM_MCE="mce"; shift ;;
        --acm-cluster-sp-repo)
            require_arg "$1" "${2:-}"
            ACM_CLUSTER_SP_REPO="${2:-}"; shift 2 ;;
        --acm-cluster-sp-branch)
            require_arg "$1" "${2:-}"
            ACM_CLUSTER_SP_BRANCH="${2:-}"; shift 2 ;;
        --kubeconfig)
            require_arg "$1" "${2:-}"
            DCM_KUBECONFIG="${2:-}"; shift 2 ;;
        --cluster-api)
            require_arg "$1" "${2:-}"
            OPENSHIFT_API="${2:-}"; shift 2 ;;
        --cluster-username)
            require_arg "$1" "${2:-}"
            OPENSHIFT_USERNAME="${2:-}"; shift 2 ;;
        --cluster-password)
            require_arg "$1" "${2:-}"
            OPENSHIFT_PASSWORD="${2:-}"; shift 2 ;;
        --compose-file)
            require_arg "$1" "${2:-}"
            COMPOSE_EXTRA_FILE_ARGS+=("-f" "$(cd "$(dirname "${2:-}")" && pwd)/$(basename "${2:-}")")
            shift 2 ;;
        --auth-enabled)
            AUTH_ENABLED_EXPLICIT=true; shift ;;
        --cleanup-on-failure)
            CLEANUP_ON_FAILURE=true; shift ;;
        --running-versions)
            RUNNING_VERSIONS=true; shift ;;
        --tear-down)
            TEAR_DOWN=true; shift ;;
        --help)
            usage; exit 0 ;;
        *)
            if match_provider_flag "$1"; then
                case "${MATCHED_TYPE}" in
                    enable)
                        PROV_ENABLED[MATCHED_IDX]=true
                        shift ;;
                    namespace)
                        require_arg "$1" "${2:-}"
                        PROV_NAMESPACES[MATCHED_IDX]="${2:-}"
                        # Keep NAMESPACE_ENV in sync so agent embeds (e.g. SP_VM_NAMESPACE
                        # via KUBEVIRT_VM_NAMESPACE) see the same value when the
                        # standalone provider profile is not enabled.
                        if [[ -n "${PROV_NS_ENVS[MATCHED_IDX]}" ]]; then
                            export "${PROV_NS_ENVS[MATCHED_IDX]}=${2:-}"
                        fi
                        shift 2 ;;
                esac
            else
                err "Unknown option: $1"
                usage; exit 1
            fi
            ;;
    esac
done

validate_deploy_dir "${CONTROL_PLANE_TMP_DIR}" || exit 1

# --- Build compose args from enabled providers ----------------------------- #

COMPOSE_PROFILES=()

any_provider_enabled() {
    local i
    for i in $(seq 0 $((PROV_COUNT - 1))); do
        [[ "${PROV_ENABLED[$i]}" == true ]] && return 0
    done
    return 1
}

for i in $(seq 0 $((PROV_COUNT - 1))); do
    [[ "${PROV_ENABLED[$i]}" == true ]] || continue
    collect_provider_compose "${i}"
done

if [[ "${GITOPS_ENABLED}" == true ]]; then
    if [[ ! -f "${GITOPS_COMPOSE_OVERRIDE}" ]]; then
        err "GitOps compose override not found: ${GITOPS_COMPOSE_OVERRIDE}"
        exit 1
    fi
    COMPOSE_EXTRA_FILE_ARGS+=("-f" "${GITOPS_COMPOSE_OVERRIDE}")
    info "Injecting dcm-gitops reconciliation container"
fi

AUTH_ENABLED=false
if [[ "${AUTH_ENABLED_EXPLICIT}" == true ]] || [[ "${AUTH_DISABLED:-}" == "false" ]]; then
    AUTH_ENABLED=true
fi

# Read the existing deployment configuration for standalone inspection and teardown.
# Do not source it: deploy/.env contains values that should not be executed as shell code.
if [[ "${RUNNING_VERSIONS}" == true || "${TEAR_DOWN}" == true ]] &&
    [[ -f "${CONTROL_PLANE_TMP_DIR}/deploy/.env" ]] &&
    grep -Eq "^AUTH_DISABLED[[:space:]]*=[[:space:]]*(false|\"false\"|'false')[[:space:]]*$" "${CONTROL_PLANE_TMP_DIR}/deploy/.env"; then
    AUTH_ENABLED=true
fi

if [[ "${AUTH_ENABLED}" == true ]]; then
    # Authentication services are defined in a separate compose file. The
    # profile only selects services from files already loaded by Compose; it
    # does not load compose.auth.yaml by itself.
    # Put the built-in auth overlay first so user/provider overrides retain
    # the documented precedence of later compose files.
    COMPOSE_EXTRA_FILE_ARGS=(
        "-f" "${CONTROL_PLANE_TMP_DIR}/deploy/compose.auth.yaml"
        "${COMPOSE_EXTRA_FILE_ARGS[@]}"
    )
    COMPOSE_PROFILES+=("--profile" "auth")
fi

# Detect environment-agent from an existing deployment for versions/teardown.
if [[ "${RUNNING_VERSIONS}" == true || "${TEAR_DOWN}" == true ]] &&
    [[ -f "${CONTROL_PLANE_TMP_DIR}/deploy/.env" ]] &&
    grep -Eq "^AGENT_EMBEDDED_SPS[[:space:]]*=" "${CONTROL_PLANE_TMP_DIR}/deploy/.env"; then
    WITH_ENVIRONMENT_AGENT=true
fi

if [[ "${WITH_ENVIRONMENT_AGENT}" == true ]]; then
    COMPOSE_PROFILES+=("--profile" "environment-agent")
fi

# --- Running versions (standalone) ----------------------------------------- #

if [[ "${RUNNING_VERSIONS}" == true ]]; then
    check_required_tools podman podman-compose curl jq || exit 1
    ensure_podman_running || exit 1
    get_running_versions "${CONTROL_PLANE_TMP_DIR}/deploy/compose.yaml" ${COMPOSE_EXTRA_FILE_ARGS[@]+"${COMPOSE_EXTRA_FILE_ARGS[@]}"} ${COMPOSE_PROFILES[@]+"${COMPOSE_PROFILES[@]}"} || exit 1
    exit 0
fi

if [[ "${TEAR_DOWN}" == true ]]; then
    ensure_podman_running || exit 1
    tear_down "${CONTROL_PLANE_TMP_DIR}" ${COMPOSE_EXTRA_FILE_ARGS[@]+"${COMPOSE_EXTRA_FILE_ARGS[@]}"} ${COMPOSE_PROFILES[@]+"${COMPOSE_PROFILES[@]}"}
    exit 0
fi

# --- Prerequisite validation ----------------------------------------------- #

log "Checking prerequisites"

REQUIRED_TOOLS=(git podman podman-compose curl jq)

for i in $(seq 0 $((PROV_COUNT - 1))); do
    [[ "${PROV_ENABLED[$i]}" == true ]] || continue
    resolve_provider_cli "${i}"
done

# Embedded cluster SP calls oc (namespace + pull-secret) after this check.
if [[ "${WITH_ENVIRONMENT_AGENT}" == true ]] && agent_embeds cluster; then
    REQUIRED_TOOLS+=(oc)
fi

check_required_tools "${REQUIRED_TOOLS[@]}" || exit 1
info "All prerequisites found: ${REQUIRED_TOOLS[*]}"

ensure_podman_running || exit 1

# Agent needs a cluster kubeconfig for the bind mount (OCP path; no Kind).
if [[ "${WITH_ENVIRONMENT_AGENT}" == true ]]; then
    if [[ -z "${AGENT_EMBEDDED_SPS}" ]]; then
        err "--with-environment-agent requires --agent-embedded-sps (e.g. container,vm)"
        exit 1
    fi

    # Reject overlapping standalone SPs that the agent already embeds.
    # Mapping: embedded name → provider flag substring / label.
    declare -A EMBEDDED_TO_STANDALONE=(
        [container]="k8s-container"
        [vm]="kubevirt"
        [cluster]="acm-cluster"
        [storage]="k8s-storage"
        [network]="k8s-network"
    )
    IFS=',' read -r -a EMBEDDED_LIST <<< "${AGENT_EMBEDDED_SPS}"
    for embedded in "${EMBEDDED_LIST[@]}"; do
        embedded="${embedded// /}"
        [[ -n "${embedded}" ]] || continue
        standalone_label="${EMBEDDED_TO_STANDALONE[${embedded}]:-}"
        if [[ -z "${standalone_label}" ]]; then
            err "Unknown embedded SP '${embedded}'. Expected: container,vm,cluster,storage,network"
            exit 1
        fi
        for i in $(seq 0 $((PROV_COUNT - 1))); do
            [[ "${PROV_ENABLED[$i]}" == true ]] || continue
            if [[ "${PROV_LABELS[$i]}" == "${standalone_label}"* ]]; then
                err "Cannot combine --with-environment-agent (embedded '${embedded}') with standalone --${PROV_FLAGS[$i]}"
                err "Either use the agent embedded SP or the standalone provider profile, not both."
                exit 1
            fi
        done
    done

    # Host port clash: agent defaults to 8081, same as compose-kubevirt-sp.yaml —
    # reject even when 'vm' is not embedded (capability overlap is handled above).
    for i in $(seq 0 $((PROV_COUNT - 1))); do
        [[ "${PROV_ENABLED[$i]}" == true ]] || continue
        [[ "${PROV_LABELS[$i]}" == kubevirt* ]] || continue
        if [[ "${AGENT_PORT}" == "${DEFAULT_AGENT_PORT}" ]]; then
            err "Cannot combine --with-environment-agent (AGENT_PORT=${AGENT_PORT}) with standalone --${PROV_FLAGS[$i]}"
            err "Both publish host port ${DEFAULT_AGENT_PORT}. Pass --agent-port with a different port (e.g. 18081)."
            exit 1
        fi
    done

    resolve_kubeconfig || exit 1
    # Compose bind mounts need an absolute host path.
    if [[ "${DCM_KUBECONFIG}" != /* ]]; then
        DCM_KUBECONFIG="$(cd "$(dirname "${DCM_KUBECONFIG}")" && pwd)/$(basename "${DCM_KUBECONFIG}")"
    fi

    # Embedded cluster SP needs namespace + pull secret before compose up
    # (standalone ACM validation is skipped on the agent path).
    if agent_embeds cluster; then
        prepare_embedded_cluster_sp "${DCM_KUBECONFIG}" || exit 1
    fi
fi

# Resolve cluster credentials only when a provider needs cluster access or ACM/MCE deploy is enabled
any_provider_needs_cluster() {
    local i
    for i in $(seq 0 $((PROV_COUNT - 1))); do
        [[ "${PROV_ENABLED[$i]}" == true ]] || continue
        [[ -n "${PROV_CLI_REQS[$i]}" ]] && return 0
    done
    return 1
}
if any_provider_needs_cluster || [[ -n "${DEPLOY_ACM_MCE}" ]]; then
    resolve_kubeconfig || exit 1
fi

# Validate and export env vars for each enabled provider
for i in $(seq 0 $((PROV_COUNT - 1))); do
    [[ "${PROV_ENABLED[$i]}" == true ]] || continue

    local_ns="${PROV_NAMESPACES[$i]}"
    local_cli="${PROV_CLIS[$i]}"
    local_hook="${PROV_VALIDATES[$i]}"

    # Cluster connectivity check (common to all providers)
    if [[ -n "${local_cli}" ]] && [[ -n "${DCM_KUBECONFIG}" ]]; then
        info "Verifying cluster connectivity for ${PROV_LABELS[$i]} (using ${local_cli})..."
        if ! "${local_cli}" --kubeconfig="${DCM_KUBECONFIG}" cluster-info &>/dev/null; then
            err "Cannot connect to cluster using kubeconfig: ${DCM_KUBECONFIG}"
            exit 1
        fi
        info "Cluster is reachable"
    fi

    # Provider-specific validation
    if [[ -n "${local_hook}" ]] && type -t "${local_hook}" &>/dev/null; then
        "${local_hook}" "${DCM_KUBECONFIG}" "${local_ns}" "${local_cli}" || exit 1
    fi

    # Export compose substitution vars
    if [[ -n "${PROV_KC_EXPORTS[$i]}" ]]; then
        export "${PROV_KC_EXPORTS[$i]}=${DCM_KUBECONFIG}"
    fi
    if [[ -n "${PROV_NS_EXPORTS[$i]}" ]]; then
        export "${PROV_NS_EXPORTS[$i]}=${local_ns}"
    fi
done

# --- ACM / MCE deployment -------------------------------------------------- #

if [[ -n "${DEPLOY_ACM_MCE}" ]]; then
    DEPLOY_LABEL="$(echo "${DEPLOY_ACM_MCE}" | tr '[:lower:]' '[:upper:]')"

    if [[ "${DEPLOY_ACM_MCE}" == "acm" ]]; then
        CR_KIND="MultiClusterHub"
        CR_API="operator.open-cluster-management.io/v1"
        CR_NAME="multiclusterhub"
        CR_NAMESPACE="open-cluster-management"
        CSV_PREFIX="advanced-cluster-management"
    else
        CR_KIND="MultiClusterEngine"
        CR_API="multicluster.openshift.io/v1"
        CR_NAME="multiclusterengine"
        CR_NAMESPACE="multicluster-engine"
        CSV_PREFIX="multicluster-engine"
    fi

    # Check if the CR already exists and is ready
    cr_phase=$(oc --kubeconfig="${DCM_KUBECONFIG}" get "${CR_KIND}" "${CR_NAME}" \
        -n "${CR_NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")

    if [[ "${cr_phase}" == "Running" ]]; then
        log "${DEPLOY_LABEL} is already installed and running — skipping deployment"
    else
        # Check if operator CSV is already installed
        csv_installed=false
        while IFS= read -r line; do
            if [[ "${line}" == *"${CSV_PREFIX}"* && "${line}" == *"Succeeded"* ]]; then
                csv_installed=true
                break
            fi
        done < <(oc --kubeconfig="${DCM_KUBECONFIG}" get csv -n "${CR_NAMESPACE}" --no-headers 2>/dev/null || true)

        if [[ "${csv_installed}" == true ]] && [[ -z "${cr_phase}" ]]; then
            # Operator is installed but CR doesn't exist yet — create it
            log "${DEPLOY_LABEL} operator is installed but ${CR_KIND} not found — creating it"
            oc --kubeconfig="${DCM_KUBECONFIG}" apply -f - <<CREOF
apiVersion: ${CR_API}
kind: ${CR_KIND}
metadata:
  name: ${CR_NAME}
  namespace: ${CR_NAMESPACE}
spec: {}
CREOF
        elif [[ "${csv_installed}" == true ]] && [[ -n "${cr_phase}" ]]; then
            # CR exists but not yet Running — just wait
            log "${DEPLOY_LABEL} ${CR_KIND} exists (phase: ${cr_phase}) — waiting for Running"
        else
            # Nothing installed — run the full upstream script
            ACM_SP_TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/dcm-acm-sp.XXXXXX")

            log "Cloning acm-cluster-service-provider (branch=${ACM_CLUSTER_SP_BRANCH})"
            git clone --branch "${ACM_CLUSTER_SP_BRANCH}" --single-branch --depth 1 \
                "${ACM_CLUSTER_SP_REPO}" "${ACM_SP_TMP_DIR}/repo"

            ACM_MCE_DEPLOY_SCRIPT="${ACM_SP_TMP_DIR}/repo/hack/deploy-acm-mce.sh"
            if [[ ! -f "${ACM_MCE_DEPLOY_SCRIPT}" ]]; then
                rm -rf "${ACM_SP_TMP_DIR}"
                err "deploy-acm-mce.sh not found in cloned repo at ${ACM_MCE_DEPLOY_SCRIPT}"
                exit 1
            fi

            log "Deploying ${DEPLOY_LABEL} on the cluster (this may take 10-20 minutes)"
            KUBECONFIG="${DCM_KUBECONFIG}" bash "${ACM_MCE_DEPLOY_SCRIPT}" "--${DEPLOY_ACM_MCE}"
            deploy_rc=$?
            rm -rf "${ACM_SP_TMP_DIR}"
            [[ ${deploy_rc} -eq 0 ]] || exit 1
        fi

        # Wait for the CR to reach Running (common path for all non-skip cases)
        if [[ "${cr_phase}" != "Running" ]]; then
            cr_timeout="${DEPLOY_TIMEOUT:-1200}"
            cr_elapsed=0
            log "Waiting for ${CR_KIND} to reach Running (timeout: ${cr_timeout}s)"
            while [[ ${cr_elapsed} -lt ${cr_timeout} ]]; do
                cr_phase=$(oc --kubeconfig="${DCM_KUBECONFIG}" get "${CR_KIND}" "${CR_NAME}" \
                    -n "${CR_NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
                if [[ "${cr_phase}" == "Running" ]]; then
                    break
                fi
                info "${CR_KIND} phase: ${cr_phase:-Pending} (${cr_elapsed}s elapsed)"
                sleep 30
                cr_elapsed=$((cr_elapsed + 30))
            done

            if [[ "${cr_phase}" == "Running" ]]; then
                log "${DEPLOY_LABEL} is ready"
            else
                err "${CR_KIND} did not reach Running within ${cr_timeout}s (last phase: ${cr_phase:-unknown})"
                exit 1
            fi
        fi
    fi
fi

# --- CNV / OpenShift Virtualization deployment ----------------------------- #

if [[ "${DEPLOY_CNV}" == true ]]; then
    CNV_NAMESPACE="openshift-cnv"
    CNV_CSV_PREFIX="kubevirt-hyperconverged"
    CNV_CR_NAME="kubevirt-hyperconverged"

    # Check if HyperConverged CR already exists and is ready
    cnv_csv_phase=$(oc --kubeconfig="${DCM_KUBECONFIG}" get csv -n "${CNV_NAMESPACE}" \
        --no-headers 2>/dev/null | awk "/${CNV_CSV_PREFIX}/{print \$NF}" | head -1)

    if [[ "${cnv_csv_phase}" == "Succeeded" ]]; then
        log "OpenShift Virtualization (CNV) is already installed (CSV: Succeeded) — skipping deployment"
    else
        log "Deploying OpenShift Virtualization (CNV) on the cluster (this may take 10-20 minutes)"

        # Create namespace
        oc --kubeconfig="${DCM_KUBECONFIG}" create namespace "${CNV_NAMESPACE}" \
            --dry-run=client -o yaml | oc --kubeconfig="${DCM_KUBECONFIG}" apply -f -

        # Create OperatorGroup
        oc --kubeconfig="${DCM_KUBECONFIG}" apply -f - <<CNVEOF
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: kubevirt-hyperconverged-group
  namespace: ${CNV_NAMESPACE}
spec:
  targetNamespaces:
    - ${CNV_NAMESPACE}
CNVEOF

        # Create Subscription
        oc --kubeconfig="${DCM_KUBECONFIG}" apply -f - <<CNVEOF
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: hco-operatorhub
  namespace: ${CNV_NAMESPACE}
spec:
  source: redhat-operators
  sourceNamespace: openshift-marketplace
  name: kubevirt-hyperconverged
  channel: "stable"
CNVEOF

        # Wait for CSV to succeed
        cnv_timeout="${DEPLOY_TIMEOUT:-1200}"
        cnv_elapsed=0
        log "Waiting for CNV CSV to reach Succeeded (timeout: ${cnv_timeout}s)"
        while [[ ${cnv_elapsed} -lt ${cnv_timeout} ]]; do
            cnv_csv_phase=$(oc --kubeconfig="${DCM_KUBECONFIG}" get csv -n "${CNV_NAMESPACE}" \
                --no-headers 2>/dev/null | awk "/${CNV_CSV_PREFIX}/{print \$NF}" | head -1)
            [[ "${cnv_csv_phase}" == "Succeeded" ]] && break
            info "CNV CSV phase: ${cnv_csv_phase:-Pending} (${cnv_elapsed}s elapsed)"
            sleep 30
            cnv_elapsed=$((cnv_elapsed + 30))
        done

        if [[ "${cnv_csv_phase}" != "Succeeded" ]]; then
            err "CNV CSV did not reach Succeeded within ${cnv_timeout}s"
            exit 1
        fi

        # Create HyperConverged CR
        oc --kubeconfig="${DCM_KUBECONFIG}" apply -f - <<CNVEOF
apiVersion: hco.kubevirt.io/v1beta1
kind: HyperConverged
metadata:
  name: ${CNV_CR_NAME}
  namespace: ${CNV_NAMESPACE}
spec: {}
CNVEOF

        # Wait for HyperConverged to be Available
        cnv_elapsed=0
        log "Waiting for HyperConverged to be Available (timeout: ${cnv_timeout}s)"
        while [[ ${cnv_elapsed} -lt ${cnv_timeout} ]]; do
            cnv_ready=$(oc --kubeconfig="${DCM_KUBECONFIG}" get hyperconverged "${CNV_CR_NAME}" \
                -n "${CNV_NAMESPACE}" -o jsonpath='{.status.conditions[?(@.type=="Available")].status}' \
                2>/dev/null || echo "")
            [[ "${cnv_ready}" == "True" ]] && break
            info "HyperConverged Available: ${cnv_ready:-Unknown} (${cnv_elapsed}s elapsed)"
            sleep 30
            cnv_elapsed=$((cnv_elapsed + 30))
        done

        if [[ "${cnv_ready}" != "True" ]]; then
            err "HyperConverged did not become Available within ${cnv_timeout}s"
            exit 1
        fi

        log "OpenShift Virtualization (CNV) is ready"
    fi
fi

# --- Version pinning ------------------------------------------------------- #

if [[ -n "${DCM_VERSION}" ]]; then
    if [[ "${DCM_VERSION}" == "release" ]]; then
        log "Resolving latest release version from Quay.io"
        DCM_VERSION=$(resolve_latest_version) || exit 1
        info "Resolved: ${DCM_VERSION}"
    fi

    if [[ "${DCM_VERSION}" != "main" ]] && [[ "${CONTROL_PLANE_BRANCH_EXPLICIT}" == false ]]; then
        RELEASE_TAG="${DCM_VERSION%%-*}"
        CONTROL_PLANE_BRANCH="release/${RELEASE_TAG}"
        info "Auto-derived control-plane branch: ${CONTROL_PLANE_BRANCH}"
    fi

    log "Pinning all DCM images to version: ${DCM_VERSION}"
    for var in "${VERSION_ENV_VARS[@]}"; do
        export "${var}=${DCM_VERSION}"
    done
fi

# --- Clone ----------------------------------------------------------------- #

log "Preparing deploy directory: ${CONTROL_PLANE_TMP_DIR}"

if [[ -d "${CONTROL_PLANE_TMP_DIR}" ]]; then
    info "Cleaning existing deploy directory..."
    podman_compose -f "${CONTROL_PLANE_TMP_DIR}/deploy/compose.yaml" ${COMPOSE_EXTRA_FILE_ARGS[@]+"${COMPOSE_EXTRA_FILE_ARGS[@]}"} ${COMPOSE_PROFILES[@]+"${COMPOSE_PROFILES[@]}"} down -v 2>/dev/null || true
    rm -rf "${CONTROL_PLANE_TMP_DIR}"
fi

log "Cloning control-plane (repo=${CONTROL_PLANE_REPO}, branch=${CONTROL_PLANE_BRANCH})"
git clone --branch "${CONTROL_PLANE_BRANCH}" --single-branch --depth 1 "${CONTROL_PLANE_REPO}" "${CONTROL_PLANE_TMP_DIR}"

ensure_deploy_env "${CONTROL_PLANE_TMP_DIR}" || exit 1

# --- Deploy ---------------------------------------------------------------- #

if [[ "${CLEANUP_ON_FAILURE}" == true ]]; then
    trap 'err "Deploy failed — cleaning up"; tear_down "${CONTROL_PLANE_TMP_DIR}" ${COMPOSE_EXTRA_FILE_ARGS[@]+"${COMPOSE_EXTRA_FILE_ARGS[@]}"} ${COMPOSE_PROFILES[@]+"${COMPOSE_PROFILES[@]}"}' ERR
fi

log "Starting DCM stack"
ENABLED_LABELS=()
for i in $(seq 0 $((PROV_COUNT - 1))); do
    [[ "${PROV_ENABLED[$i]}" == true ]] && ENABLED_LABELS+=("${PROV_LABELS[$i]}")
done
if [[ ${#ENABLED_LABELS[@]} -gt 0 ]]; then
    info "Enabled providers: ${ENABLED_LABELS[*]}"
fi
if [[ "${WITH_ENVIRONMENT_AGENT}" == true ]]; then
    info "Environment agent enabled (embedded SPs: ${AGENT_EMBEDDED_SPS})"
fi
if [[ "${AUTH_ENABLED}" == true ]]; then
    info "Authentication enabled (compose profile: auth)"
fi
# Single bring-up: platform services + optional --profile environment-agent together.
podman_compose -f "${CONTROL_PLANE_TMP_DIR}/deploy/compose.yaml" ${COMPOSE_EXTRA_FILE_ARGS[@]+"${COMPOSE_EXTRA_FILE_ARGS[@]}"} ${COMPOSE_PROFILES[@]+"${COMPOSE_PROFILES[@]}"} up -d

echo
log "Container status"
podman_compose -f "${CONTROL_PLANE_TMP_DIR}/deploy/compose.yaml" ${COMPOSE_EXTRA_FILE_ARGS[@]+"${COMPOSE_EXTRA_FILE_ARGS[@]}"} ${COMPOSE_PROFILES[@]+"${COMPOSE_PROFILES[@]}"} ps

verify_health "${CONTROL_PLANE_TMP_DIR}/deploy/compose.yaml" ${COMPOSE_EXTRA_FILE_ARGS[@]+"${COMPOSE_EXTRA_FILE_ARGS[@]}"} ${COMPOSE_PROFILES[@]+"${COMPOSE_PROFILES[@]}"} || exit 1

get_running_versions "${CONTROL_PLANE_TMP_DIR}/deploy/compose.yaml" ${COMPOSE_EXTRA_FILE_ARGS[@]+"${COMPOSE_EXTRA_FILE_ARGS[@]}"} ${COMPOSE_PROFILES[@]+"${COMPOSE_PROFILES[@]}"} || info "Version collection failed (non-fatal)"

GATEWAY_URL="http://localhost:${CONTROL_PLANE_PORT}"
log "DCM stack is up and healthy at ${GATEWAY_URL}"
if [[ "${WITH_ENVIRONMENT_AGENT}" == true ]]; then
    info "Environment agent API: http://localhost:${AGENT_PORT}"
fi
if [[ "${CONTROL_PLANE_TMP_DIR}" != "${DEFAULT_CONTROL_PLANE_TMP_DIR}" ]]; then
    info "To tear down: $(basename "$0") --control-plane-dir ${CONTROL_PLANE_TMP_DIR} --tear-down"
else
    info "To tear down: $(basename "$0") --tear-down"
fi
