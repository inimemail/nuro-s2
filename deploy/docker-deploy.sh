#!/bin/bash
# =============================================================================
# Sub2API Docker Deployment Preparation Script
# =============================================================================
# This script prepares deployment files for Sub2API:
#   - Downloads docker-compose.local.yml and .env.example
#   - Generates secure secrets (JWT_SECRET, TOTP_ENCRYPTION_KEY, POSTGRES_PASSWORD)
#   - Creates necessary data directories
#
# After running this script, you can start services with:
#   docker-compose up -d
# =============================================================================

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# GitHub raw content base URL
GITHUB_RAW_URL="https://raw.githubusercontent.com/Wei-Shaw/sub2api/main/deploy"

# Print colored message
print_info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

print_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

print_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

print_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Generate random secret
generate_secret() {
    local secret
    secret="$(openssl rand -hex 32)" || return 1
    [[ "$secret" =~ ^[a-f0-9]{64}$ ]] || return 1
    printf '%s\n' "$secret"
}

# Check if command exists
command_exists() {
    command -v "$1" >/dev/null 2>&1
}

# Main installation function
main() (
    echo ""
    echo "=========================================="
    echo "  Sub2API Deployment Preparation"
    echo "=========================================="
    echo ""

    # Check if openssl is available
    if ! command_exists openssl; then
        print_error "openssl is not installed. Please install openssl first."
        exit 1
    fi

    # Check if deployment already exists
    if [ -f ".env" ]; then
        print_info "Existing .env preserved. Use the existing deployment configuration to start services."
        return 0
    fi

    local download_dir
    download_dir="$(mktemp -d)" || { print_error "无法创建下载临时目录"; exit 1; }
    local tmp_env=""
    trap 'rm -rf -- "$download_dir"; if [ -n "$tmp_env" ]; then rm -f -- "$tmp_env"; fi' EXIT

    # Download both inputs away from their final paths. Validate the env
    # contract before generating credentials so a broken/HTML template cannot
    # produce a successful-looking deployment.
    print_info "Downloading docker-compose.yml..."
    if command_exists curl; then
        curl -fsSL "${GITHUB_RAW_URL}/docker-compose.local.yml" -o "$download_dir/docker-compose.yml"
    elif command_exists wget; then
        wget -q "${GITHUB_RAW_URL}/docker-compose.local.yml" -O "$download_dir/docker-compose.yml"
    else
        print_error "Neither curl nor wget is installed. Please install one of them."
        exit 1
    fi
    grep -Eq '^[[:space:]]*services:' "$download_dir/docker-compose.yml" || {
        print_error "下载的 docker-compose.yml 缺少 services 配置"; exit 1;
    }
    print_success "Downloaded docker-compose.yml"

    # Download .env.example
    print_info "Downloading .env.example..."
    if command_exists curl; then
        curl -fsSL "${GITHUB_RAW_URL}/.env.example" -o "$download_dir/.env.example"
    else
        wget -q "${GITHUB_RAW_URL}/.env.example" -O "$download_dir/.env.example"
    fi
    for required_key in JWT_SECRET TOTP_ENCRYPTION_KEY POSTGRES_PASSWORD ADMIN_EMAIL; do
        grep -Eq "^${required_key}=" "$download_dir/.env.example" || {
            print_error "下载的 .env.example 缺少 ${required_key} 配置"; exit 1;
        }
    done
    mv -f -- "$download_dir/docker-compose.yml" docker-compose.yml
    mv -f -- "$download_dir/.env.example" .env.example
    print_success "Downloaded .env.example"

    # Generate .env file with auto-generated secrets
    print_info "Generating secure secrets..."
    echo ""

    # Generate secrets
    JWT_SECRET=$(generate_secret) || { print_error "Failed to generate JWT secret"; return 1; }
    TOTP_ENCRYPTION_KEY=$(generate_secret) || { print_error "Failed to generate TOTP key"; return 1; }
    POSTGRES_PASSWORD=$(generate_secret) || { print_error "Failed to generate database password"; return 1; }
    local admin_suffix
    admin_suffix="$(openssl rand -hex 6)" || { print_error "Failed to generate admin email"; return 1; }
    [[ "$admin_suffix" =~ ^[a-f0-9]{12}$ ]] || { print_error "Invalid random admin email"; return 1; }
    ADMIN_EMAIL="admin-${admin_suffix}@sub2api.local"

    # Commit .env only after every replacement and filesystem operation succeeds.
    tmp_env="$(mktemp ./.env.tmp.XXXXXX)"
    (umask 077; cat .env.example > "$tmp_env")

    # Update .env with generated secrets (cross-platform compatible)
    if sed --version >/dev/null 2>&1; then
        # GNU sed (Linux)
        sed -i "s/^JWT_SECRET=.*/JWT_SECRET=${JWT_SECRET}/" "$tmp_env"
        sed -i "s/^TOTP_ENCRYPTION_KEY=.*/TOTP_ENCRYPTION_KEY=${TOTP_ENCRYPTION_KEY}/" "$tmp_env"
        sed -i "s/^POSTGRES_PASSWORD=.*/POSTGRES_PASSWORD=${POSTGRES_PASSWORD}/" "$tmp_env"
        sed -i "s/^ADMIN_EMAIL=.*/ADMIN_EMAIL=${ADMIN_EMAIL}/" "$tmp_env"
    else
        # BSD sed (macOS)
        sed -i '' "s/^JWT_SECRET=.*/JWT_SECRET=${JWT_SECRET}/" "$tmp_env"
        sed -i '' "s/^TOTP_ENCRYPTION_KEY=.*/TOTP_ENCRYPTION_KEY=${TOTP_ENCRYPTION_KEY}/" "$tmp_env"
        sed -i '' "s/^POSTGRES_PASSWORD=.*/POSTGRES_PASSWORD=${POSTGRES_PASSWORD}/" "$tmp_env"
        sed -i '' "s/^ADMIN_EMAIL=.*/ADMIN_EMAIL=${ADMIN_EMAIL}/" "$tmp_env"
    fi

    # Create data directories
    print_info "Creating data directories..."
    mkdir -p data postgres_data redis_data
    print_success "Created data directories"

    # Set secure permissions for .env file (readable/writable only by owner)
    chmod 600 "$tmp_env"
    mv -f -- "$tmp_env" .env
    tmp_env=""
    echo ""

    # Display completion message
    echo "=========================================="
    echo "  Preparation Complete!"
    echo "=========================================="
    echo ""
    echo "Generated secure credentials:"
    echo "  ADMIN_EMAIL:           ${ADMIN_EMAIL}"
    echo "  POSTGRES_PASSWORD:     ${POSTGRES_PASSWORD}"
    echo "  JWT_SECRET:            ${JWT_SECRET}"
    echo "  TOTP_ENCRYPTION_KEY:   ${TOTP_ENCRYPTION_KEY}"
    echo ""
    print_warning "These credentials have been saved to .env file."
    print_warning "Please keep them secure and do not share publicly!"
    echo ""
    echo "Directory structure:"
    echo "  docker-compose.yml        - Docker Compose configuration"
    echo "  .env                      - Environment variables (generated secrets)"
    echo "  .env.example              - Example template (for reference)"
    echo "  data/                     - Application data (will be created on first run)"
    echo "  postgres_data/            - PostgreSQL data"
    echo "  redis_data/               - Redis data"
    echo ""
    echo "Next steps:"
    echo "  1. (Optional) Edit .env to customize configuration"
    echo "  2. Start services:"
    echo "     docker-compose up -d"
    echo ""
    echo "  3. View logs:"
    echo "     docker-compose logs -f sub2api"
    echo ""
    echo "  4. Access Web UI:"
    echo "     http://localhost:8080"
    echo ""
    print_info "If admin password is not set in .env, it will be auto-generated."
    print_info "Check logs for the generated admin password on first startup."
    echo ""
)

# Run main function
main "$@"
