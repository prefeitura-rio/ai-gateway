# Testing the DANFE Endpoint

This directory contains test scripts to validate the `/api/v1/enqueue/danfe` endpoint implementation.

## Prerequisites

### For Python Tests
- Python 3.7+
- Required packages:
  ```bash
  pip install requests colorama
  ```

### For Bash Tests
- bash (Git Bash on Windows, or native on Linux/macOS)
- curl
- jq (optional, for pretty JSON output)

## Configuration

1. **Copy the environment template:**
   ```bash
   cp .env.test .env
   ```

2. **Edit `.env` with your settings:**
   ```bash
   AI_GATEWAY_URL=http://localhost:8000
   API_TOKEN=  # Optional: Add your bearer token if auth is required
   ```

3. **Or set environment variables directly:**
   ```bash
   export AI_GATEWAY_URL=http://localhost:8000
   export API_TOKEN=your-token-here  # Optional
   ```

## Running the Tests

### Option 1: Python Test Suite (Recommended)

**Run all tests:**
```bash
python test_danfe_endpoint.py
```

**Or with environment variables inline:**
```bash
AI_GATEWAY_URL=http://localhost:8000 python test_danfe_endpoint.py
```

**Expected output:**
```
================================================================================
                     AI-GATEWAY DANFE ENDPOINT TEST SUITE
================================================================================

Gateway URL: http://localhost:8000
Test Endpoint: http://localhost:8000/api/v1/enqueue/danfe
Authentication: Disabled

================================================================================
                              HEALTH CHECK
================================================================================

✓ Gateway is healthy at http://localhost:8000

================================================================================
                              RUNNING TESTS
================================================================================

Test: Valid DANFE enqueue request
  Status Code: 201
  Response: {
    "status": "enqueued",
    "queue": "danfe_processing",
    "message": "DANFE processing request enqueued successfully"
  }
✓ Got expected status code 201
✓ Response has correct status: 'enqueued'
✓ Response confirms queue: 'danfe_processing'
...
```

### Option 2: Bash/Curl Test Suite

**Make the script executable:**
```bash
chmod +x test_danfe_endpoint.sh
```

**Run all tests:**
```bash
./test_danfe_endpoint.sh
```

**Or with environment variables:**
```bash
AI_GATEWAY_URL=http://localhost:8000 ./test_danfe_endpoint.sh
```

### Option 3: Manual Testing with curl

**Test a valid request:**
```bash
curl -X POST http://localhost:8000/api/v1/enqueue/danfe \
  -H "Content-Type: application/json" \
  -d '{
    "user_number": "5521999999999",
    "message": "gs://my-bucket/danfe-files/invoice-123.pdf",
    "metadata": {
      "tipo_processamento": "danfe",
      "solicitacao_id": "12345"
    },
    "provider": "vertex_ai",
    "callback_url": "https://example.com/webhook/callback"
  }'
```

**Expected response (201 Created):**
```json
{
  "status": "enqueued",
  "queue": "danfe_processing",
  "message": "DANFE processing request enqueued successfully"
}
```

## Test Scenarios

The test suites cover the following scenarios:

### ✅ Valid Requests (expect 201 Created)

1. **Valid request with all fields**
   - user_number, message, metadata, provider, callback_url

2. **Minimal request**
   - Only required fields: user_number, message

3. **Request with extensive metadata**
   - Complex nested metadata structure

### ❌ Invalid Requests (expect 400 Bad Request)

1. **Invalid callback URL - localhost**
   - Should reject: `http://localhost:8080/callback`

2. **Invalid callback URL - private IP**
   - Should reject: `http://192.168.1.100/callback`

3. **Missing required field**
   - Missing user_number

4. **Empty message field**
   - message: ""

5. **Malformed JSON**
   - Invalid JSON syntax

## Validating the Message Queue

To verify that messages are actually being enqueued to RabbitMQ:

### Using RabbitMQ Management UI

1. Access: http://localhost:15672 (default credentials: guest/guest)
2. Navigate to "Queues" tab
3. Look for queue: `danfe_processing`
4. Check message count increases after running tests

### Using RabbitMQ CLI

```bash
# List all queues
docker exec -it rabbitmq rabbitmqctl list_queues

# Check danfe_processing queue
docker exec -it rabbitmq rabbitmqctl list_queues name messages | grep danfe
```

### Expected Queue Topology

After running the gateway, you should see these queues:
- `danfe_processing` - Main DANFE processing queue
- `danfe_processing_dlq` - Dead Letter Queue for failed messages
- `user_messages` - Existing queue (not affected)
- `user_messages_dlq` - DLQ for user messages

## Verifying Worker Isolation

To confirm that gateway workers don't consume DANFE messages:

1. **Send a test message:**
   ```bash
   python test_danfe_endpoint.py
   ```

2. **Check RabbitMQ queue:**
   - `danfe_processing` should have messages
   - Messages should NOT be consumed by gateway workers

3. **Check gateway worker logs:**
   ```bash
   docker logs ai-gateway-worker
   ```
   - Should only show processing from `user_messages` queue
   - Should NOT show any `danfe_processing` consumption

4. **Expected behavior:**
   - Gateway workers: Consume only `user_messages`
   - DANFE workers: Will consume `danfe_processing` (when implemented in Fase 2)

## Troubleshooting

### Gateway not responding
```
Error: Cannot connect to gateway at http://localhost:8000
```

**Solution:**
1. Check if gateway is running:
   ```bash
   docker ps | grep ai-gateway
   # or
   just health
   ```

2. Verify the URL is correct:
   ```bash
   curl http://localhost:8000/health
   ```

### Tests failing with 500 errors

**Check gateway logs:**
```bash
docker logs ai-gateway
# or
just logs
```

**Common issues:**
- RabbitMQ not running or not accessible
- Redis not running or not accessible
- Configuration issues in .env file

### Tests failing with 400 errors on valid requests

**Possible causes:**
1. Validation rules changed
2. Missing required fields in test payload
3. Gateway authentication enabled but token not provided

**Debug:**
```bash
# Run with verbose curl output
curl -v -X POST http://localhost:8000/api/v1/enqueue/danfe \
  -H "Content-Type: application/json" \
  -d '{"user_number": "123", "message": "test"}'
```

## Integration with CI/CD

### GitHub Actions Example

```yaml
- name: Install test dependencies
  run: pip install requests colorama

- name: Run DANFE endpoint tests
  env:
    AI_GATEWAY_URL: http://localhost:8000
  run: python test_danfe_endpoint.py
```

### Docker Compose Testing

```bash
# Start services
docker-compose up -d

# Wait for services to be ready
sleep 10

# Run tests
docker-compose exec ai-gateway python test_danfe_endpoint.py

# Cleanup
docker-compose down
```

## Next Steps

After validating the endpoint:

1. ✅ **Fase 1 Complete** - AI-Gateway adjustments done
2. 🔄 **Fase 2** - Implement DANFE worker consumer (app-danfe-worker)
3. 🧪 **End-to-End Testing** - Test full flow from API → Queue → Worker → Result

## Support

If tests are failing or you encounter issues:

1. Check the gateway health endpoint: `/health`
2. Verify RabbitMQ is running and accessible
3. Check gateway logs for errors
4. Ensure all required environment variables are set
5. Verify network connectivity between services

For more information, see:
- Main README: `README.md`
- Implementation plan: `../danfe-ai/PLANO_IMPLEMENTACAO_CONSUMER.md`
- CLAUDE.md for development guidelines
