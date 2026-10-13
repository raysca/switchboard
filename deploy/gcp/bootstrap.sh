#!/usr/bin/env bash
# One-time Google Cloud setup for the Cloud Run deployment.
# Idempotent: re-running adds what is missing and NEVER rotates an existing secret.
# DRY_RUN=1 prints every command and runs none (and never prints a secret value).
#
# Required environment:
#   PROJECT_ID  GITHUB_REPO (owner/name)
#   XAI_API_KEY  ELEVENLABS_API_KEY  GOOGLE_API_KEY   provider keys (Gemini uses GOOGLE_API_KEY)
#   GOOGLE_SA_KEY_FILE   path to the service-account JSON for Google speech (STT/TTS)
# Optional: REGION (us-central1)  AR_REPO (govoice)  BILLING_ACCOUNT + BUDGET_USD (50)  for a budget alert
set -euo pipefail

need() { [[ -n "${!1:-}" ]] || { echo "bootstrap: $1 is required" >&2; exit 2; }; }
for v in PROJECT_ID GITHUB_REPO XAI_API_KEY ELEVENLABS_API_KEY GOOGLE_API_KEY GOOGLE_SA_KEY_FILE; do need "$v"; done
[[ "$GITHUB_REPO" =~ ^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$ ]] || { echo "bootstrap: GITHUB_REPO must look like owner/name" >&2; exit 2; }
[[ -r "$GOOGLE_SA_KEY_FILE" ]] || { echo "bootstrap: GOOGLE_SA_KEY_FILE is not a readable file" >&2; exit 2; }

REGION="${REGION:-us-central1}"
AR_REPO="${AR_REPO:-govoice}"
RUNTIME_SA="govoice-runtime@${PROJECT_ID}.iam.gserviceaccount.com"
DEPLOYER_SA="govoice-deployer@${PROJECT_ID}.iam.gserviceaccount.com"
DRY="${DRY_RUN:-}"

# run prints the command and executes it unless DRY_RUN is set. Secrets never appear in arguments.
run() { echo "+ $*"; if [[ -z "$DRY" ]]; then "$@"; fi; }
# exists succeeds when the command succeeds; a dry run assumes nothing exists.
exists() { [[ -z "$DRY" ]] && "$@" >/dev/null 2>&1; }

echo "== project ${PROJECT_ID}, region ${REGION}, repo ${GITHUB_REPO} =="
if [[ -n "$DRY" ]]; then PROJECT_NUMBER="000000000000"; else PROJECT_NUMBER="$(gcloud projects describe "$PROJECT_ID" --format='value(projectNumber)')"; fi

echo "== APIs =="
run gcloud services enable run.googleapis.com artifactregistry.googleapis.com secretmanager.googleapis.com \
  iamcredentials.googleapis.com iam.googleapis.com cloudresourcemanager.googleapis.com sts.googleapis.com --project "$PROJECT_ID"

echo "== Artifact Registry =="
exists gcloud artifacts repositories describe "$AR_REPO" --location "$REGION" --project "$PROJECT_ID" ||
  run gcloud artifacts repositories create "$AR_REPO" --repository-format=docker --location "$REGION" --project "$PROJECT_ID"

echo "== service accounts =="
for sa in govoice-runtime govoice-deployer; do
  exists gcloud iam service-accounts describe "${sa}@${PROJECT_ID}.iam.gserviceaccount.com" --project "$PROJECT_ID" ||
    run gcloud iam service-accounts create "$sa" --display-name "$sa" --project "$PROJECT_ID"
done

echo "== secrets (created once; never rotated here) =="
# put_secret NAME SOURCE: SOURCE is "env:VAR", "file:PATH" or "random". The value goes in on stdin or from a file.
put_secret() {
  local name="$1" src="$2" var
  if exists gcloud secrets describe "$name" --project "$PROJECT_ID"; then echo "secret $name exists, left unchanged"; return; fi
  echo "+ create secret $name (from ${src%%:*})"
  [[ -z "$DRY" ]] || return 0
  gcloud secrets create "$name" --replication-policy=automatic --project "$PROJECT_ID" >/dev/null
  case "$src" in
    env:*)  var="${src#env:}"; printf %s "${!var}" | gcloud secrets versions add "$name" --data-file=- --project "$PROJECT_ID" >/dev/null ;;
    file:*) gcloud secrets versions add "$name" --data-file="${src#file:}" --project "$PROJECT_ID" >/dev/null ;;
    random) openssl rand -hex 32 | tr -d '\n' | gcloud secrets versions add "$name" --data-file=- --project "$PROJECT_ID" >/dev/null ;;
  esac
}
put_secret XAI_API_KEY env:XAI_API_KEY
put_secret ELEVENLABS_API_KEY env:ELEVENLABS_API_KEY
put_secret GOOGLE_API_KEY env:GOOGLE_API_KEY
put_secret GOOGLE_SA_KEY "file:${GOOGLE_SA_KEY_FILE}"
put_secret TOKEN_SECRET random
put_secret GOVOICE_MANAGEMENT_API_KEY random
put_secret DENTAL_MCP_TOKEN random

echo "== who may read what =="
for s in XAI_API_KEY ELEVENLABS_API_KEY GOOGLE_API_KEY GOOGLE_SA_KEY TOKEN_SECRET GOVOICE_MANAGEMENT_API_KEY DENTAL_MCP_TOKEN; do
  run gcloud secrets add-iam-policy-binding "$s" --member="serviceAccount:${RUNTIME_SA}" --role=roles/secretmanager.secretAccessor --project "$PROJECT_ID" --condition=None
done
# The deployer reads only the management key (the smoke test logs in with it); never a provider key.
run gcloud secrets add-iam-policy-binding GOVOICE_MANAGEMENT_API_KEY --member="serviceAccount:${DEPLOYER_SA}" --role=roles/secretmanager.secretAccessor --project "$PROJECT_ID" --condition=None

echo "== deployer permissions =="
run gcloud projects add-iam-policy-binding "$PROJECT_ID" --member="serviceAccount:${DEPLOYER_SA}" --role=roles/run.admin --condition=None
run gcloud artifacts repositories add-iam-policy-binding "$AR_REPO" --location "$REGION" --member="serviceAccount:${DEPLOYER_SA}" --role=roles/artifactregistry.writer --project "$PROJECT_ID"
run gcloud iam service-accounts add-iam-policy-binding "$RUNTIME_SA" --member="serviceAccount:${DEPLOYER_SA}" --role=roles/iam.serviceAccountUser --project "$PROJECT_ID"

echo "== Workload Identity Federation: only ${GITHUB_REPO} on main =="
exists gcloud iam workload-identity-pools describe github --location=global --project "$PROJECT_ID" ||
  run gcloud iam workload-identity-pools create github --location=global --display-name="GitHub Actions" --project "$PROJECT_ID"
exists gcloud iam workload-identity-pools providers describe github-oidc --location=global --workload-identity-pool=github --project "$PROJECT_ID" ||
  run gcloud iam workload-identity-pools providers create-oidc github-oidc --location=global --workload-identity-pool=github \
    --issuer-uri="https://token.actions.githubusercontent.com" \
    --attribute-mapping="google.subject=assertion.sub,attribute.repository=assertion.repository,attribute.ref=assertion.ref" \
    --attribute-condition="assertion.repository=='${GITHUB_REPO}' && assertion.ref=='refs/heads/main'" \
    --project "$PROJECT_ID"
POOL="projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/github"
run gcloud iam service-accounts add-iam-policy-binding "$DEPLOYER_SA" --role=roles/iam.workloadIdentityUser \
  --member="principalSet://iam.googleapis.com/${POOL}/attribute.repository/${GITHUB_REPO}" --project "$PROJECT_ID"
WIF_PROVIDER="${POOL}/providers/github-oidc"

if [[ -n "${BILLING_ACCOUNT:-}" ]]; then
  echo "== budget alert =="
  run gcloud billing budgets create --billing-account="$BILLING_ACCOUNT" --display-name="govoice-demo" \
    --budget-amount="${BUDGET_USD:-50}USD" --threshold-rule=percent=0.5 --threshold-rule=percent=0.9 --threshold-rule=percent=1.0 \
    --filter-projects="projects/${PROJECT_ID}"
else
  echo "== budget alert skipped (set BILLING_ACCOUNT to create one) =="
fi

echo "== GitHub repository variables (not secrets) =="
if command -v gh >/dev/null 2>&1 && [[ -z "$DRY" ]]; then
  gh variable set GCP_PROJECT_ID --body "$PROJECT_ID" --repo "$GITHUB_REPO"
  gh variable set GCP_REGION --body "$REGION" --repo "$GITHUB_REPO"
  gh variable set GCP_WIF_PROVIDER --body "$WIF_PROVIDER" --repo "$GITHUB_REPO"
  gh variable set GCP_DEPLOYER_SA --body "$DEPLOYER_SA" --repo "$GITHUB_REPO"
else
  echo "+ gh variable set GCP_PROJECT_ID / GCP_REGION / GCP_WIF_PROVIDER / GCP_DEPLOYER_SA --repo $GITHUB_REPO"
fi
echo "done. The admin key: gcloud secrets versions access latest --secret=GOVOICE_MANAGEMENT_API_KEY --project $PROJECT_ID"
