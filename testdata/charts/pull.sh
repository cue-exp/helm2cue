#!/bin/bash
# Pull bitnami/nginx chart for integration testing.
# Extracted files are committed to the repo for reproducibility.
set -euo pipefail

CHART_VERSION="22.0.7"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DEST_DIR="$SCRIPT_DIR/nginx"

helm repo add bitnami https://charts.bitnami.com/bitnami
helm repo update bitnami

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

helm pull bitnami/nginx --version "$CHART_VERSION" --untar --untardir "$TMPDIR"

rm -rf "$DEST_DIR"
mkdir -p "$DEST_DIR/templates"

cp "$TMPDIR/nginx/Chart.yaml" "$DEST_DIR/"
cp "$TMPDIR/nginx/values.yaml" "$DEST_DIR/"
cp "$TMPDIR/nginx/templates/"* "$DEST_DIR/templates/"

# Copy dependency charts (needed for helm template to work).
if [ -d "$TMPDIR/nginx/charts" ]; then
	cp -r "$TMPDIR/nginx/charts" "$DEST_DIR/charts"
fi

echo "Pulled bitnami/nginx $CHART_VERSION into $DEST_DIR"
