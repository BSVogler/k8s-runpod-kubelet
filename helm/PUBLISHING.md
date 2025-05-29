# Publishing the Helm Chart to GHCR

## Automatic Publishing (GitHub Actions)

The chart is automatically published to GitHub Container Registry when:
- You push changes to the `helm/` directory on the `master` branch
- You create a new release

## Manual Publishing

To manually publish the chart:

1. Create a GitHub Personal Access Token with `write:packages` permission:
   - Go to GitHub Settings → Developer settings → Personal access tokens
   - Create a new token with `write:packages` scope. I do not find the packages scope in the new token so I use the classic token creation page.

2. Export your token:
   ```bash
   export GITHUB_TOKEN=your_token_here
   ```

3. Run the publish script:
   ```bash
   ./helm/publish-ghcr.sh
   ```

## Versioning

Before publishing a new version:
1. Update the `version` field in `helm/runpod-kubelet/Chart.yaml`
2. Update the `appVersion` if the application version has changed
3. Commit your changes

## Package Visibility

After first publish, you may need to:
1. Go to your GitHub profile → Packages
2. Find the `charts/runpod-kubelet` package
3. Go to Package settings
4. Change visibility to Public (if desired)
5. Link the package to your repository