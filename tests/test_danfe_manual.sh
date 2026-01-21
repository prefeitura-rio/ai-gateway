#!/bin/bash
# Quick manual test for DANFE endpoint
# Single curl command for quick validation

GATEWAY_URL="${AI_GATEWAY_URL:-http://localhost:8000}"

echo "Testing DANFE endpoint at: ${GATEWAY_URL}/api/v1/enqueue/danfe"
echo ""

curl -v -X POST "${GATEWAY_URL}/api/v1/enqueue/danfe" \
  -H "Content-Type: application/json" \
  -d '{
    "user_number": "5521999999999",
    "message": "gs://my-bucket/danfe-files/test-invoice.pdf",
    "metadata": {
      "tipo_processamento": "danfe",
      "solicitacao_id": "test-123",
      "test": true
    },
    "provider": "vertex_ai",
    "callback_url": "https://example.com/webhook/callback"
  }' | jq . 2>/dev/null || cat

echo ""
echo ""
echo "Expected response (201 Created):"
echo '{'
echo '  "status": "enqueued",'
echo '  "queue": "danfe_processing",'
echo '  "message": "DANFE processing request enqueued successfully"'
echo '}'
