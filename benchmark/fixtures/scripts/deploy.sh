#!/bin/bash
# deploy.sh — Deploy widget-api to target environment
# Usage: ./deploy.sh [staging|production]

set -euo pipefail

TARGET_ENV="${1:-staging}"
TARGET_HOST="deploy-staging.internal"
TARGET_PORT="22"
APP_DIR="/opt/widget-api"
ARTIFACT="widget-api-2.4.1.tar.gz"

echo "Deploying $ARTIFACT to $TARGET_ENV ($TARGET_HOST:$TARGET_PORT)"
echo "App directory: $APP_DIR"

# Validate
if [[ "$TARGET_ENV" != "staging" && "$TARGET_ENV" != "production" ]]; then
    echo "Error: unknown environment '$TARGET_ENV'"
    exit 1
fi

# Simulate deploy steps
echo "1. Uploading artifact..."
echo "2. Stopping service..."
echo "3. Extracting to $APP_DIR..."
echo "4. Running migrations..."
echo "5. Starting service..."
echo "Deploy complete."
