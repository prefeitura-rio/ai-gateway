#!/bin/bash
# Test script for /api/v1/enqueue/danfe endpoint
# Simple curl-based tests for the DANFE enqueue endpoint

set -e

# Configuration
GATEWAY_URL="${AI_GATEWAY_URL:-http://localhost:8000}"
ENDPOINT="${GATEWAY_URL}/api/v1/enqueue/danfe"
API_TOKEN="${API_TOKEN:-}"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color

# Test counters
TOTAL_TESTS=0
PASSED_TESTS=0
FAILED_TESTS=0

print_header() {
    echo -e "\n${CYAN}================================================================================${NC}"
    echo -e "${CYAN}$(printf '%80s' "$1" | tr ' ' ' ')${NC}" | sed 's/^/                    /'
    echo -e "${CYAN}================================================================================${NC}\n"
}

print_test() {
    echo -e "${YELLOW}Test: $1${NC}"
}

print_success() {
    echo -e "${GREEN}✓ $1${NC}"
}

print_error() {
    echo -e "${RED}✗ $1${NC}"
}

print_info() {
    echo -e "${BLUE}  $1${NC}"
}

# Build curl command with optional auth
curl_cmd() {
    local method="$1"
    local url="$2"
    local data="$3"

    if [ -n "$API_TOKEN" ]; then
        curl -s -w "\n%{http_code}" -X "$method" "$url" \
            -H "Content-Type: application/json" \
            -H "Authorization: Bearer $API_TOKEN" \
            -d "$data"
    else
        curl -s -w "\n%{http_code}" -X "$method" "$url" \
            -H "Content-Type: application/json" \
            -d "$data"
    fi
}

run_test() {
    local test_name="$1"
    local payload="$2"
    local expected_status="$3"

    TOTAL_TESTS=$((TOTAL_TESTS + 1))
    print_test "$test_name"

    # Make request and capture response + status code
    response=$(curl_cmd POST "$ENDPOINT" "$payload")

    # Extract status code (last line)
    status_code=$(echo "$response" | tail -n1)

    # Extract body (all but last line)
    body=$(echo "$response" | head -n-1)

    print_info "Status Code: $status_code"
    print_info "Response: $(echo "$body" | jq -C . 2>/dev/null || echo "$body")"

    # Check status code
    if [ "$status_code" = "$expected_status" ]; then
        print_success "Got expected status code $expected_status"
        PASSED_TESTS=$((PASSED_TESTS + 1))
        return 0
    else
        print_error "Expected $expected_status, got $status_code"
        FAILED_TESTS=$((FAILED_TESTS + 1))
        return 1
    fi
}

check_dependencies() {
    if ! command -v curl &> /dev/null; then
        echo -e "${RED}Error: curl is not installed${NC}"
        exit 1
    fi

    if ! command -v jq &> /dev/null; then
        echo -e "${YELLOW}Warning: jq is not installed (optional, for pretty JSON output)${NC}"
    fi
}

check_gateway_health() {
    print_header "HEALTH CHECK"

    echo "Gateway URL: $GATEWAY_URL"
    echo "Test Endpoint: $ENDPOINT"
    echo "Authentication: $([ -n "$API_TOKEN" ] && echo "Enabled" || echo "Disabled")"
    echo ""

    health_url="${GATEWAY_URL}/health"

    if ! health_response=$(curl -s -w "\n%{http_code}" "$health_url" 2>&1); then
        print_error "Cannot connect to gateway at $GATEWAY_URL"
        print_info "Make sure the ai-gateway service is running"
        return 1
    fi

    health_status=$(echo "$health_response" | tail -n1)
    health_body=$(echo "$health_response" | head -n-1)

    if [ "$health_status" = "200" ]; then
        print_success "Gateway is healthy at $GATEWAY_URL"
        print_info "Health status: $(echo "$health_body" | jq -C . 2>/dev/null || echo "$health_body")"
        return 0
    else
        print_error "Gateway health check failed: $health_status"
        return 1
    fi
}

print_summary() {
    print_header "TEST SUMMARY"

    echo "Total tests: $TOTAL_TESTS"
    echo -e "${GREEN}Passed: $PASSED_TESTS${NC}"
    echo -e "${RED}Failed: $FAILED_TESTS${NC}"

    echo -e "\n${CYAN}================================================================================${NC}"
}

# Test Cases

test_valid_request() {
    local payload='{
        "user_number": "5521999999999",
        "message": "gs://my-bucket/danfe-files/invoice-123.pdf",
        "metadata": {
            "tipo_processamento": "danfe",
            "solicitacao_id": "12345",
            "usuario_id": "user-67890",
            "test": true
        },
        "provider": "vertex_ai",
        "callback_url": "https://example.com/webhook/danfe-callback"
    }'

    run_test "Valid DANFE enqueue request" "$payload" "201"
}

test_minimal_request() {
    local payload='{
        "user_number": "5521999999999",
        "message": "gs://my-bucket/danfe-files/invoice-minimal.pdf"
    }'

    run_test "Minimal request (only required fields)" "$payload" "201"
}

test_invalid_callback_url_localhost() {
    local payload='{
        "user_number": "5521999999999",
        "message": "gs://my-bucket/danfe-files/invoice-456.pdf",
        "callback_url": "http://localhost:8080/callback"
    }'

    run_test "Invalid callback URL - localhost (should fail)" "$payload" "400"
}

test_invalid_callback_url_private_ip() {
    local payload='{
        "user_number": "5521999999999",
        "message": "gs://my-bucket/danfe-files/invoice-789.pdf",
        "callback_url": "http://192.168.1.100/callback"
    }'

    run_test "Invalid callback URL - private IP (should fail)" "$payload" "400"
}

test_missing_required_field() {
    local payload='{
        "message": "gs://my-bucket/danfe-files/invoice-missing-user.pdf"
    }'

    run_test "Missing required field - user_number (should fail)" "$payload" "400"
}

test_empty_message() {
    local payload='{
        "user_number": "5521999999999",
        "message": ""
    }'

    run_test "Empty message field (should fail)" "$payload" "400"
}

test_with_metadata() {
    local payload='{
        "user_number": "danfe-worker",
        "message": "gs://danfe-storage/2024/01/invoice-complex.pdf",
        "metadata": {
            "tipo_processamento": "danfe",
            "solicitacao_id": 98765,
            "usuario_id": 12345,
            "empresa_id": 100,
            "tipo_documento": "NFe",
            "ambiente": "producao",
            "prioridade": "alta"
        },
        "provider": "vertex_ai"
    }'

    run_test "Request with extensive metadata" "$payload" "201"
}

# Main execution
main() {
    print_header "AI-GATEWAY DANFE ENDPOINT TEST SUITE"

    check_dependencies

    if ! check_gateway_health; then
        echo -e "\n${RED}Gateway is not available. Aborting tests.${NC}"
        exit 1
    fi

    print_header "RUNNING TESTS"

    test_valid_request
    sleep 0.5

    test_minimal_request
    sleep 0.5

    test_with_metadata
    sleep 0.5

    test_invalid_callback_url_localhost
    sleep 0.5

    test_invalid_callback_url_private_ip
    sleep 0.5

    test_missing_required_field
    sleep 0.5

    test_empty_message
    sleep 0.5

    print_summary

    # Exit with appropriate code
    [ "$FAILED_TESTS" -eq 0 ] && exit 0 || exit 1
}

main
