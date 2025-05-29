#!/bin/bash
# Script to manually package and publish Helm chart to GitHub Container Registry

set -e

# Get chart version
CHART_VERSION=$(grep '^version:' ./helm/runpod-kubelet/Chart.yaml | awk '{print $2}')
GITHUB_OWNER="${GITHUB_OWNER:-bsvogler}"

echo "Packaging Helm chart version $CHART_VERSION..."

# Package the chart
helm package ./helm/runpod-kubelet

# Push to GHCR
echo "Pushing chart to ghcr.io/${GITHUB_OWNER}/helm/runpod-kubelet..."
helm push runpod-kubelet-${CHART_VERSION}.tgz oci://ghcr.io/${GITHUB_OWNER}/helm

echo "Chart published successfully!"
echo "Users can install it with:"
echo "helm install runpod-kubelet oci://ghcr.io/${GITHUB_OWNER}/helm/runpod-kubelet --version ${CHART_VERSION}"