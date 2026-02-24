#!/bin/bash
# Pull kube-prometheus-stack chart for integration testing.
# Extracted files are committed to the repo for reproducibility.
set -euo pipefail

CHART_VERSION="82.2.1"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DEST_DIR="$SCRIPT_DIR/kube-prometheus-stack"

helm repo add prometheus-community https://prometheus-community.github.io/helm-charts 2>/dev/null || true
helm repo update prometheus-community

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

helm pull prometheus-community/kube-prometheus-stack --version "$CHART_VERSION" --untar --untardir "$TMPDIR"

SRC="$TMPDIR/kube-prometheus-stack"

rm -rf "$DEST_DIR"
mkdir -p "$DEST_DIR"

cp "$SRC/Chart.yaml" "$DEST_DIR/"
cp "$SRC/values.yaml" "$DEST_DIR/"

# Copy templates tree (including subdirectories).
cp -r "$SRC/templates" "$DEST_DIR/templates"

# Copy dependency charts (needed for helpers).
if [ -d "$SRC/charts" ]; then
	cp -r "$SRC/charts" "$DEST_DIR/charts"
fi

echo "Pulled kube-prometheus-stack $CHART_VERSION into $DEST_DIR"
