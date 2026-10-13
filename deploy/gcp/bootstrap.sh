#!/usr/bin/env bash
# One-time Google Cloud setup for the Cloud Run deployment.
# Idempotent: re-running adds what is missing and NEVER rotates an existing secret.
# DRY_RUN=1 prints every command and runs none (and never prints a secret value).
#
# Required environment:
#   PROJECT_ID  GITHUB_REPO (owner/name)
#   XAI_API_KEY  ELEVENLABS_API_KEY  GOOGLE_API_KEY   provider keys (Gemini uses GOOGLE_API_KEY)
#   GOOGLE_SA_KEY_FILE   path to the service-account JSON for Google speech (STT/TTS)
# Optional: REGION (us-central1)  AR_REPO (govoice)  BILLING_ACCOUNT + BUDGET_USD (150)  for a budget alert
set -euo pipefail

need() { [[ -n "${!1:-}" ]] || { echo "bootstrap: $1 is required" >&2; exit 2; }; }
for v in PROJECT_ID GITHUB_REPO XAI_API_KEY ELEVENLABS_API_KEY GOOGLE_API_KEY GOOGLE_SA_KEY_FILE; do need "$v"; done
[[ "$GITHUB_REPO" =~ ^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$ ]] || { echo "bootstrap: GITHUB_REPO must look like owner/name" >&2; exit 2; }
[[ -r "$GOOGLE_SA_KEY_FILE" ]] || { echo "bootstrap: GOOGLE_SA_KEY_FILE is not a readable file" >&2; exit 2; }

REGION="${REGION:-us-central1}"
AR_REPO="${AR_REPO:-govoice}"
RUNTIME_SA="govoice-runtime@${PROJECT_ID}.iam.gserviceaccount.com"
MCP_SA="dental-mcp-runtime@${PROJECT_ID}.iam.gserviceaccount.com"   # the public MCP service: one secret only
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
  iamcredentials.googleapis.com iam.googleapis.com cloudresourcemanager.googleapis.com sts.googleapis.com \
  billingbudgets.googleapis.com --project "$PROJECT_ID"

echo "== Artifact Registry =="
exists gcloud artifacts repositories describe "$AR_REPO" --location "$REGION" --project "$PROJECT_ID" ||
  run gcloud artifacts repositories create "$AR_REPO" --repository-format=docker --location "$REGION" --project "$PROJECT_ID"

echo "== service accounts =="
for sa in govoice-runtime dental-mcp-runtime govoice-deployer; do
  exists gcloud iam service-accounts describe "${sa}@${PROJECT_ID}.iam.gserviceaccount.com" --project "$PROJECT_ID" ||
    run gcloud iam service-accounts create "$sa" --display-name "$sa" --project "$PROJECT_ID"
done

echo "== secrets (created once; never rotated here) =="
# put_secret NAME SOURCE: SOURCE is "env:VAR", "file:PATH" or "random". The value goes in on stdin or from a file.
# A secret that exists WITH an enabled version is left alone; one left without a version by an interrupted run gets one.
put_secret() {
  local name="$1" src="$2" var
  if exists gcloud secrets describe "$name" --project "$PROJECT_ID"; then
    if [[ -n "$(gcloud secrets versions list "$name" --filter=state=enabled --format='value(name)' --project "$PROJECT_ID" 2>/dev/null)" ]]; then
      echo "secret $name exists, left unchanged"; return
    fi
    echo "secret $name exists without a version: adding one"
  else
    echo "+ create secret $name (from ${src%%:*})"
    [[ -z "$DRY" ]] || return 0
    gcloud secrets create "$name" --replication-policy=automatic --project "$PROJECT_ID" >/dev/null
  fi
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
# The public MCP service runs as its own account and reads only its bearer token.
run gcloud secrets add-iam-policy-binding DENTAL_MCP_TOKEN --member="serviceAccount:${MCP_SA}" --role=roles/secretmanager.secretAccessor --project "$PROJECT_ID" --condition=None
# The deployer reads only the management key (the smoke test logs in with it); never a provider key.
run gcloud secrets add-iam-policy-binding GOVOICE_MANAGEMENT_API_KEY --member="serviceAccount:${DEPLOYER_SA}" --role=roles/secretmanager.secretAccessor --project "$PROJECT_ID" --condition=None

echo "== deployer permissions =="
run gcloud projects add-iam-policy-binding "$PROJECT_ID" --member="serviceAccount:${DEPLOYER_SA}" --role=roles/run.admin --condition=None
run gcloud artifacts repositories add-iam-policy-binding "$AR_REPO" --location "$REGION" --member="serviceAccount:${DEPLOYER_SA}" --role=roles/artifactregistry.writer --project "$PROJECT_ID"
for sa in "$RUNTIME_SA" "$MCP_SA"; do
  run gcloud iam service-accounts add-iam-policy-binding "$sa" --member="serviceAccount:${DEPLOYER_SA}" --role=roles/iam.serviceAccountUser --project "$PROJECT_ID"
done

echo "== Workload Identity Federation: only ${GITHUB_REPO} on main =="
exists gcloud iam workload-identity-pools describe github --location=global --project "$PROJECT_ID" ||
  run gcloud iam workload-identity-pools create github --location=global --display-name="GitHub Actions" --project "$PROJECT_ID"
WIF_ARGS=(--location=global --workload-identity-pool=github
  --issuer-uri="https://token.actions.githubusercontent.com"
  --attribute-mapping="google.subject=assertion.sub,attribute.repository=assertion.repository,attribute.ref=assertion.ref"
  --attribute-condition="assertion.repository=='${GITHUB_REPO}' && assertion.ref=='refs/heads/main'"
  --project "$PROJECT_ID")
if exists gcloud iam workload-identity-pools providers describe github-oidc --location=global --workload-identity-pool=github --project "$PROJECT_ID"; then
  run gcloud iam workload-identity-pools providers update-oidc github-oidc "${WIF_ARGS[@]}"   # a changed GITHUB_REPO must not be ignored
else
  run gcloud iam workload-identity-pools providers create-oidc github-oidc "${WIF_ARGS[@]}"
fi
POOL="projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/github"
run gcloud iam service-accounts add-iam-policy-binding "$DEPLOYER_SA" --role=roles/iam.workloadIdentityUser \
  --member="principalSet://iam.googleapis.com/${POOL}/attribute.repository/${GITHUB_REPO}" --project "$PROJECT_ID"
WIF_PROVIDER="${POOL}/providers/github-oidc"

echo "== GitHub repository variables (not secrets) =="
if command -v gh >/dev/null 2>&1 && [[ -z "$DRY" ]]; then
  gh variable set GCP_PROJECT_ID --body "$PROJECT_ID" --repo "$GITHUB_REPO"
  gh variable set GCP_REGION --body "$REGION" --repo "$GITHUB_REPO"
  gh variable set GCP_WIF_PROVIDER --body "$WIF_PROVIDER" --repo "$GITHUB_REPO"
  gh variable set GCP_DEPLOYER_SA --body "$DEPLOYER_SA" --repo "$GITHUB_REPO"
else
  echo "+ gh variable set GCP_PROJECT_ID / GCP_REGION / GCP_WIF_PROVIDER / GCP_DEPLOYER_SA --repo $GITHUB_REPO"
fi
if [[ -n "${BILLING_ACCOUNT:-}" ]]; then
  echo "== budget alert (a warm 2 vCPU/2 GiB instance alone is ~100 USD/month: the default is 150) =="
  if [[ -z "$DRY" ]] && [[ -n "$(gcloud billing budgets list --billing-account="$BILLING_ACCOUNT" --filter='displayName=govoice-demo' --format='value(name)' 2>/dev/null)" ]]; then
    echo "budget govoice-demo exists, left unchanged"
  else
    run gcloud billing budgets create --billing-account="$BILLING_ACCOUNT" --display-name="govoice-demo" \
      --budget-amount="${BUDGET_USD:-150}USD" --threshold-rule=percent=0.5 --threshold-rule=percent=0.9 --threshold-rule=percent=1.0 \
      --filter-projects="projects/${PROJECT_ID}" || echo "WARNING: the budget was not created (billing permissions?): create it by hand" >&2
  fi
else
  echo "== budget alert skipped (set BILLING_ACCOUNT to create one) =="
fi
echo "done. The admin key: gcloud secrets versions access latest --secret=GOVOICE_MANAGEMENT_API_KEY --project $PROJECT_ID"
