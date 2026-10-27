#!/usr/bin/env bash
# Build, push and deploy the Smile Dental demo MCP service to Cloud Run.
# Usage: PROJECT_ID=demo-proj [REGION=us-central1] [AR_REPO=switchboard] [IMAGE_TAG=sha] \
#          demos/dental/deploy/cloudrun/deploy.sh
# Requires an authenticated gcloud (the deploy-demo workflow authenticates with Workload Identity
# Federation) and Docker with buildx. The bearer token is never read here: it lives in the
# DENTAL_MCP_TOKEN Secret Manager secret and is mounted by the manifest. Prints the MCP URL and,
# when GITHUB_OUTPUT is set, appends `mcp_url=` to it.
set -euo pipefail

: "${PROJECT_ID:?PROJECT_ID is required}"
REGION="${REGION:-us-central1}"
AR_REPO="${AR_REPO:-switchboard}"
IMAGE_TAG="${IMAGE_TAG:-$(git rev-parse --short HEAD)}"
SERVICE="dental-mcp"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"

if [[ -n "${DRY_RUN:-}" ]]; then
  echo "+ docker build --platform linux/amd64 -f ${ROOT}/demos/dental/deploy/cloudrun/Dockerfile.mcp"
  echo "+ docker push ${REGION}-docker.pkg.dev/${PROJECT_ID}/${AR_REPO}/${SERVICE}:${IMAGE_TAG}"
  echo "+ gcloud run services replace demos/dental/deploy/cloudrun/cloudrun.yaml --region ${REGION} --project ${PROJECT_ID}"
  echo "+ wait for /health, print the service URL + /mcp"
  exit 0
fi

DEMO_RUNTIME_SA="demo-mcp-runtime@${PROJECT_ID}.iam.gserviceaccount.com"
IMAGE="${REGION}-docker.pkg.dev/${PROJECT_ID}/${AR_REPO}/${SERVICE}:${IMAGE_TAG}"
MANIFEST="$(mktemp)"
trap 'rm -f "${MANIFEST}"' EXIT

echo "== build ${IMAGE} (linux/amd64) =="
docker build --platform linux/amd64 -q -f "${ROOT}/demos/dental/deploy/cloudrun/Dockerfile.mcp" -t "${IMAGE}" "${ROOT}"
docker push "${IMAGE}"

echo "== deploy ${SERVICE} =="
REGION="${REGION}" PROJECT_ID="${PROJECT_ID}" IMAGE="${IMAGE}" DEMO_RUNTIME_SA="${DEMO_RUNTIME_SA}" \
  envsubst '${REGION} ${PROJECT_ID} ${IMAGE} ${DEMO_RUNTIME_SA}' \
  < "${ROOT}/demos/dental/deploy/cloudrun/cloudrun.yaml" > "${MANIFEST}"
gcloud run services replace "${MANIFEST}" --region "${REGION}" --project "${PROJECT_ID}"
# Public HTTPS: the clean-room server accepts public MCP destinations only, and the MCP bearer
# token from Secret Manager is what protects the tools.
gcloud run services add-iam-policy-binding "${SERVICE}" --region "${REGION}" --project "${PROJECT_ID}" \
  --member=allUsers --role=roles/run.invoker >/dev/null

URL="$(gcloud run services describe "${SERVICE}" --region "${REGION}" --project "${PROJECT_ID}" --format='value(status.url)')"
BASE="${URL%/}"
for _ in $(seq 1 60); do
  curl -fsS --max-time 5 "${BASE}/health" >/dev/null 2>&1 && break || sleep 2
done
curl -fsS --max-time 10 "${BASE}/health" >/dev/null || {
  echo "deploy.sh: ${SERVICE} did not become healthy at ${BASE}/health" >&2
  exit 1
}
echo "MCP_URL=${BASE}/mcp"
if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
  echo "mcp_url=${BASE}/mcp" >> "${GITHUB_OUTPUT}"
fi
