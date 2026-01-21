#!/usr/bin/env python3
"""
Test script for /api/v1/enqueue/danfe endpoint
Tests the DANFE enqueue endpoint with various scenarios
"""

import os
import sys
import json
import time
from typing import Dict, Any, Optional
import requests
from colorama import init, Fore, Style

# Initialize colorama for colored output
init(autoreset=True)

# Configuration
GATEWAY_URL = os.getenv("AI_GATEWAY_URL", "http://localhost:8000")
API_TOKEN = os.getenv("API_TOKEN", "")  # If authentication is required
ENDPOINT = f"{GATEWAY_URL}/api/v1/enqueue/danfe"

# Test results tracking
test_results = []


def print_header(text: str):
    """Print a formatted header"""
    print(f"\n{Fore.CYAN}{'=' * 80}")
    print(f"{Fore.CYAN}{text:^80}")
    print(f"{Fore.CYAN}{'=' * 80}{Style.RESET_ALL}\n")


def print_test(test_name: str):
    """Print test name"""
    print(f"{Fore.YELLOW}Test: {test_name}{Style.RESET_ALL}")


def print_success(message: str):
    """Print success message"""
    print(f"{Fore.GREEN}✓ {message}{Style.RESET_ALL}")


def print_error(message: str):
    """Print error message"""
    print(f"{Fore.RED}✗ {message}{Style.RESET_ALL}")


def print_info(message: str):
    """Print info message"""
    print(f"{Fore.BLUE}  {message}{Style.RESET_ALL}")


def send_request(
    payload: Dict[str, Any],
    expected_status: int = 201,
    test_name: str = "",
) -> tuple[bool, Optional[Dict]]:
    """
    Send a POST request to the DANFE endpoint

    Args:
        payload: Request payload
        expected_status: Expected HTTP status code
        test_name: Name of the test for reporting

    Returns:
        Tuple of (success, response_data)
    """
    headers = {"Content-Type": "application/json"}
    if API_TOKEN:
        headers["Authorization"] = f"Bearer {API_TOKEN}"

    try:
        response = requests.post(ENDPOINT, json=payload, headers=headers, timeout=10)

        print_info(f"Status Code: {response.status_code}")

        try:
            response_data = response.json()
            print_info(f"Response: {json.dumps(response_data, indent=2)}")
        except json.JSONDecodeError:
            response_data = None
            print_info(f"Response (text): {response.text}")

        # Check status code
        if response.status_code == expected_status:
            print_success(f"Got expected status code {expected_status}")
            success = True
        else:
            print_error(f"Expected {expected_status}, got {response.status_code}")
            success = False

        # Record test result
        test_results.append({
            "test": test_name,
            "success": success,
            "status_code": response.status_code,
            "expected_status": expected_status,
        })

        return success, response_data

    except requests.exceptions.RequestException as e:
        print_error(f"Request failed: {e}")
        test_results.append({
            "test": test_name,
            "success": False,
            "error": str(e),
        })
        return False, None


def test_valid_request():
    """Test with a valid DANFE request"""
    print_test("Valid DANFE enqueue request")

    payload = {
        "user_number": "5521999999999",
        "message": "gs://my-bucket/danfe-files/invoice-123.pdf",
        "metadata": {
            "tipo_processamento": "danfe",
            "solicitacao_id": "12345",
            "usuario_id": "user-67890",
            "test": True,
        },
        "provider": "vertex_ai",
        "callback_url": "https://example.com/webhook/danfe-callback",
    }

    success, response = send_request(payload, expected_status=201, test_name="Valid Request")

    if success and response:
        # Validate response structure
        if "status" in response and response["status"] == "enqueued":
            print_success("Response has correct status: 'enqueued'")
        else:
            print_error("Response missing 'status' field or incorrect value")

        if "queue" in response and response["queue"] == "danfe_processing":
            print_success("Response confirms queue: 'danfe_processing'")
        else:
            print_error("Response missing 'queue' field or incorrect value")

        if "message" in response:
            print_success(f"Response message: {response['message']}")
        else:
            print_error("Response missing 'message' field")


def test_minimal_request():
    """Test with minimal required fields only"""
    print_test("Minimal request (only required fields)")

    payload = {
        "user_number": "5521999999999",
        "message": "gs://my-bucket/danfe-files/invoice-minimal.pdf",
    }

    send_request(payload, expected_status=201, test_name="Minimal Request")


def test_invalid_callback_url_localhost():
    """Test with invalid callback URL (localhost)"""
    print_test("Invalid callback URL - localhost (should fail)")

    payload = {
        "user_number": "5521999999999",
        "message": "gs://my-bucket/danfe-files/invoice-456.pdf",
        "callback_url": "http://localhost:8080/callback",
    }

    success, response = send_request(payload, expected_status=400, test_name="Invalid Callback URL - Localhost")

    if success and response:
        if "error" in response:
            print_success(f"Got expected error: {response['error']}")


def test_invalid_callback_url_private_ip():
    """Test with invalid callback URL (private IP)"""
    print_test("Invalid callback URL - private IP (should fail)")

    payload = {
        "user_number": "5521999999999",
        "message": "gs://my-bucket/danfe-files/invoice-789.pdf",
        "callback_url": "http://192.168.1.100/callback",
    }

    send_request(payload, expected_status=400, test_name="Invalid Callback URL - Private IP")


def test_missing_required_field():
    """Test with missing required field"""
    print_test("Missing required field - user_number (should fail)")

    payload = {
        "message": "gs://my-bucket/danfe-files/invoice-missing-user.pdf",
    }

    send_request(payload, expected_status=400, test_name="Missing Required Field")


def test_empty_message():
    """Test with empty message field"""
    print_test("Empty message field (should fail)")

    payload = {
        "user_number": "5521999999999",
        "message": "",
    }

    send_request(payload, expected_status=400, test_name="Empty Message")


def test_invalid_json():
    """Test with malformed JSON"""
    print_test("Malformed JSON payload (should fail)")

    headers = {"Content-Type": "application/json"}
    if API_TOKEN:
        headers["Authorization"] = f"Bearer {API_TOKEN}"

    try:
        response = requests.post(
            ENDPOINT,
            data='{"user_number": "123", invalid json}',
            headers=headers,
            timeout=10,
        )

        print_info(f"Status Code: {response.status_code}")

        if response.status_code == 400:
            print_success("Got expected 400 Bad Request for malformed JSON")
            test_results.append({
                "test": "Malformed JSON",
                "success": True,
                "status_code": response.status_code,
            })
        else:
            print_error(f"Expected 400, got {response.status_code}")
            test_results.append({
                "test": "Malformed JSON",
                "success": False,
                "status_code": response.status_code,
            })

    except requests.exceptions.RequestException as e:
        print_error(f"Request failed: {e}")
        test_results.append({
            "test": "Malformed JSON",
            "success": False,
            "error": str(e),
        })


def test_with_metadata():
    """Test with rich metadata"""
    print_test("Request with extensive metadata")

    payload = {
        "user_number": "danfe-worker",
        "message": "gs://danfe-storage/2024/01/invoice-complex.pdf",
        "metadata": {
            "tipo_processamento": "danfe",
            "solicitacao_id": 98765,
            "usuario_id": 12345,
            "empresa_id": 100,
            "tipo_documento": "NFe",
            "ambiente": "producao",
            "prioridade": "alta",
            "tags": ["urgent", "high-value"],
            "custom_data": {
                "nested": "value",
                "number": 42,
            },
        },
        "provider": "vertex_ai",
    }

    send_request(payload, expected_status=201, test_name="Request with Metadata")


def print_summary():
    """Print test results summary"""
    print_header("TEST SUMMARY")

    total_tests = len(test_results)
    passed_tests = sum(1 for r in test_results if r.get("success", False))
    failed_tests = total_tests - passed_tests

    print(f"Total tests: {total_tests}")
    print(f"{Fore.GREEN}Passed: {passed_tests}{Style.RESET_ALL}")
    print(f"{Fore.RED}Failed: {failed_tests}{Style.RESET_ALL}")

    if failed_tests > 0:
        print(f"\n{Fore.RED}Failed tests:{Style.RESET_ALL}")
        for result in test_results:
            if not result.get("success", False):
                test_name = result.get("test", "Unknown")
                error = result.get("error", "Status code mismatch")
                print(f"  - {test_name}: {error}")

    print(f"\n{Fore.CYAN}{'=' * 80}{Style.RESET_ALL}")

    return failed_tests == 0


def check_gateway_health():
    """Check if the gateway is running and healthy"""
    print_header("HEALTH CHECK")

    try:
        health_url = f"{GATEWAY_URL}/health"
        response = requests.get(health_url, timeout=5)

        if response.status_code == 200:
            print_success(f"Gateway is healthy at {GATEWAY_URL}")
            try:
                health_data = response.json()
                print_info(f"Health status: {json.dumps(health_data, indent=2)}")
            except:
                pass
            return True
        else:
            print_error(f"Gateway health check failed: {response.status_code}")
            return False

    except requests.exceptions.RequestException as e:
        print_error(f"Cannot connect to gateway at {GATEWAY_URL}")
        print_error(f"Error: {e}")
        print_info("Make sure the ai-gateway service is running")
        print_info(f"Expected URL: {GATEWAY_URL}")
        return False


def main():
    """Main test execution"""
    print_header("AI-GATEWAY DANFE ENDPOINT TEST SUITE")

    print(f"Gateway URL: {GATEWAY_URL}")
    print(f"Test Endpoint: {ENDPOINT}")
    print(f"Authentication: {'Enabled' if API_TOKEN else 'Disabled'}")

    # Check if gateway is healthy
    if not check_gateway_health():
        print_error("\nGateway is not available. Aborting tests.")
        sys.exit(1)

    print_header("RUNNING TESTS")

    # Run all tests
    test_valid_request()
    time.sleep(0.5)

    test_minimal_request()
    time.sleep(0.5)

    test_with_metadata()
    time.sleep(0.5)

    test_invalid_callback_url_localhost()
    time.sleep(0.5)

    test_invalid_callback_url_private_ip()
    time.sleep(0.5)

    test_missing_required_field()
    time.sleep(0.5)

    test_empty_message()
    time.sleep(0.5)

    test_invalid_json()
    time.sleep(0.5)

    # Print summary
    all_passed = print_summary()

    # Exit with appropriate code
    sys.exit(0 if all_passed else 1)


if __name__ == "__main__":
    main()
