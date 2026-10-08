#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
# Evaluate trusted declarations only; omit the root-only interactive menu.
eval "$(sed '/^if \[\[ "${1:-}" == "run-backup" \]\]; then/,$d' "$repo_root/deploy-nuro.sh")"

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT
create_env_file "$workdir" 5100
email="$(read_env_value "$workdir/.env" ADMIN_EMAIL)"
password="$(read_env_value "$workdir/.env" ADMIN_PASSWORD)"
[[ "$email" =~ ^admin-[a-f0-9]{12}@nuro-sub2api.local$ ]]
[[ "$password" =~ ^[a-f0-9]{24}$ ]]
permissions="$(ls -l "$workdir/.env")"
[[ "$permissions" == -rw-------* ]]
before="$(cksum "$workdir/.env")"
create_env_file "$workdir" 5200
[[ "$(cksum "$workdir/.env")" == "$before" ]]
literal_env="$workdir/literal.env"
literal_secret='custom\nsecret\twith\\slashes'
printf 'TEST_SECRET=old\nOTHER_SETTING=preserved\n' > "$literal_env"
set_env_value "$literal_env" TEST_SECRET "$literal_secret"
[[ "$(read_env_value "$literal_env" TEST_SECRET)" == "$literal_secret" ]]
[[ "$(read_env_value "$literal_env" OTHER_SETTING)" == preserved ]]
[[ "$(wc -l < "$literal_env" | tr -d ' ')" == 2 ]]
get_local_ip() { printf '127.0.0.1'; }
access="$(show_access "$workdir")"
[[ "$access" == *"管理员账号: ${email}"* ]]
[[ "$access" == *"管理员密码: ${password}"* ]]
[[ "$access" != *"请查看"* ]]

openssl() { return 1; }
if generate_admin_password || generate_admin_email; then
    echo 'Random generation failure must be rejected' >&2
    exit 1
fi
mkdir "$workdir/failure"
if create_env_file "$workdir/failure" 5100; then
    echo 'Failed randomness must prevent .env creation' >&2
    exit 1
fi
[[ ! -e "$workdir/failure/.env" ]]
for enabled in true false; do
    env_file="$workdir/edge-$enabled.env"
    printf 'NURO_EDGE_ENABLED=%s\n' "$enabled" > "$env_file"
    if ensure_edge_env_values "$env_file"; then
        echo 'Failed randomness must stop missing Edge secret initialization' >&2
        exit 1
    fi
    [[ -z "$(read_env_value "$env_file" GATEWAY_OPENAI_EDGE_RS_INTERNAL_SECRET)" ]]
done
unset -f openssl
openssl() { printf 'invalid-random-output'; }
if generate_admin_password || generate_admin_email || generate_secret; then
    echo 'Malformed random output must be rejected' >&2
    exit 1
fi
unset -f openssl
(cd "$workdir" && bash "$repo_root/deploy/docker-deploy.sh")
[[ "$(cksum "$workdir/.env")" == "$before" ]]
export bootstrap_test_root="$repo_root"
curl() {
    case "$2" in
        */docker-compose.local.yml) cp "$bootstrap_test_root/deploy/docker-compose.local.yml" "$4" ;;
        */.env.example) cp "$bootstrap_test_root/deploy/.env.example" "$4" ;;
        *) return 22 ;;
    esac
}
export -f curl
mkdir "$workdir/common-new"
(cd "$workdir/common-new" && bash "$repo_root/deploy/docker-deploy.sh" > /dev/null)
[[ "$(read_env_value "$workdir/common-new/.env" ADMIN_EMAIL)" =~ ^admin-[a-f0-9]{12}@sub2api.local$ ]]
permissions="$(ls -l "$workdir/common-new/.env")"
[[ "$permissions" == -rw-------* ]]
curl() { return 22; }
export -f curl
mkdir "$workdir/download-failure"
if (cd "$workdir/download-failure" && bash "$repo_root/deploy/docker-deploy.sh" > /dev/null); then
    echo 'Download failures must stop deployment preparation' >&2
    exit 1
fi
[[ ! -e "$workdir/download-failure/.env" ]]

curl() {
    case "$2" in
        */docker-compose.local.yml) cp "$bootstrap_test_root/deploy/docker-compose.local.yml" "$4" ;;
        */.env.example) printf 'POSTGRES_PASSWORD=broken\n' > "$4" ;;
        *) return 22 ;;
    esac
}
export -f curl
mkdir "$workdir/malformed-template"
if (cd "$workdir/malformed-template" && bash "$repo_root/deploy/docker-deploy.sh" > /dev/null); then
    echo 'Malformed env templates must stop deployment preparation' >&2
    exit 1
fi
[[ ! -e "$workdir/malformed-template/.env" ]]
[[ ! -e "$workdir/malformed-template/docker-compose.yml" ]]

curl() {
    case "$2" in
        */docker-compose.local.yml) cp "$bootstrap_test_root/deploy/docker-compose.local.yml" "$4" ;;
        */.env.example) cp "$bootstrap_test_root/deploy/.env.example" "$4" ;;
        *) return 22 ;;
    esac
}
export -f curl
sed() { return 1; }
export -f sed
mkdir "$workdir/common-write-failure"
if (cd "$workdir/common-write-failure" && bash "$repo_root/deploy/docker-deploy.sh" > /dev/null); then
    echo 'Failed env replacements must stop deployment preparation' >&2
    exit 1
fi
[[ ! -e "$workdir/common-write-failure/.env" ]]
[[ -z "$(find "$workdir/common-write-failure" -maxdepth 1 -name '.env.tmp.*' -print -quit)" ]]
unset -f sed

mkdir "$workdir/scheduler-write-failure"
if (
    eval "$(declare -f set_env_value | sed '1s/set_env_value/original_set_env_value/')"
    set_env_value() {
        [[ "$2" != SERVER_TRUSTED_PROXIES ]] || return 1
        original_set_env_value "$@"
    }
    create_env_file "$workdir/scheduler-write-failure" 5100
); then
    echo 'An early scheduler config write failure must propagate' >&2
    exit 1
fi
[[ ! -e "$workdir/scheduler-write-failure/.env" ]]
[[ -z "$(find "$workdir/scheduler-write-failure" -maxdepth 1 -name '.env.tmp.*' -print -quit)" ]]

# A post-generation migration failure must also leave no destination file, so
# retrying installation is possible and cannot mistake a partial config for a
# completed one.
mkdir "$workdir/atomic-failure"
ensure_scheduler_env_values() { return 1; }
if create_env_file "$workdir/atomic-failure" 5100; then
    echo 'Atomic env creation must fail when post-generation migration fails' >&2
    exit 1
fi
[[ ! -e "$workdir/atomic-failure/.env" ]]
[[ -z "$(find "$workdir/atomic-failure" -maxdepth 1 -name '.env.tmp.*' -print -quit)" ]]
unset -f curl
echo 'admin bootstrap deployment tests passed'
