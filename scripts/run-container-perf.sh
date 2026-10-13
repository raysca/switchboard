#!/usr/bin/env bash
# run-container-perf.sh: Run capacity, churn, and chaos tests against a containerized
# Switchboard server running under fixed resource constraints (2 CPUs, 2GB RAM)
# with zero cloud provider costs (fakeproviders).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${ROOT_DIR}"

COMPOSE_FILE="deploy/docker-compose.perf.yaml"
REPORT_DIR="${ROOT_DIR}/reports"
mkdir -p "${REPORT_DIR}"

cleanup() {
  echo "==> Tearing down perf container..."
  docker compose -f "${COMPOSE_FILE}" down -v || true
}
trap cleanup EXIT

echo "=========================================================="
echo " Starting Switchboard Container Performance & Chaos Suite"
echo " Resource Limits: 2.0 CPUs, 2048 MB RAM (deploy.resources)"
echo " Provider Cost: \$0.00 (built with -tags fakeproviders)"
echo "=========================================================="

echo "==> Starting container..."
docker compose -f "${COMPOSE_FILE}" up -d --build

echo "==> Waiting for container readiness on http://127.0.0.1:9092/ready..."
READY=0
for i in $(seq 1 40); do
  if curl -sf http://127.0.0.1:9092/ready >/dev/null 2>&1; then
    READY=1
    echo "==> Server is READY (attempt ${i})!"
    break
  fi
  sleep 1
done

if [ "${READY}" -ne 1 ]; then
  echo "ERROR: Server failed to become ready in 40s. Logs:"
  docker compose -f "${COMPOSE_FILE}" logs
  exit 1
fi

echo ""
echo "=========================================================="
echo " Test 1: Capacity Sweep (Measuring Connections vs Resources)"
echo "=========================================================="
go run ./tools/loadtest \
  -target-url 127.0.0.1:8080 \
  -admin-url http://127.0.0.1:9090 \
  -metrics-url http://127.0.0.1:9092 \
  -container govoice-perf \
  -find-capacity \
  -cap-steps 4,8,16,24 \
  -step-duration 20s \
  -md-out "${REPORT_DIR}/capacity-container.md" \
  -json-out "${REPORT_DIR}/capacity-container.json"

echo ""
echo "=========================================================="
echo " Test 2: Simultaneous Call Churn (30s - 60s per call)"
echo "=========================================================="
go run ./tools/loadtest \
  -target-url 127.0.0.1:8080 \
  -admin-url http://127.0.0.1:9090 \
  -metrics-url http://127.0.0.1:9092 \
  -container govoice-perf \
  -sessions 16 \
  -duration 90s \
  -min-session-dur 30s \
  -max-session-dur 60s \
  -min-turns 4 \
  -max-turns 8 \
  -md-out "${REPORT_DIR}/churn-container.md" \
  -json-out "${REPORT_DIR}/churn-container.json"

echo ""
echo "=========================================================="
echo " Test 3: Fault Injection & Chaos (Malformed frames, TCP drops)"
echo "=========================================================="
go run ./tools/loadtest \
  -target-url 127.0.0.1:8080 \
  -admin-url http://127.0.0.1:9090 \
  -metrics-url http://127.0.0.1:9092 \
  -container govoice-perf \
  -sessions 12 \
  -duration 60s \
  -chaos-rate 0.20 \
  -min-turns 2 \
  -max-turns 5 \
  -md-out "${REPORT_DIR}/chaos-container.md" \
  -json-out "${REPORT_DIR}/chaos-container.json"

echo ""
echo "=========================================================="
echo " Post-Test Container Health & Fault Isolation Check"
echo "=========================================================="
if curl -sf http://127.0.0.1:9092/ready >/dev/null 2>&1; then
  echo "Container Status: HEALTHY (HTTP 200 OK after full chaos suite)"
else
  echo "ERROR: Container is NOT healthy after chaos suite!"
  exit 1
fi

echo ""
echo "All tests completed successfully. Reports written to ${REPORT_DIR}/"
