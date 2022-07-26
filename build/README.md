# Build Images

Any custom changes made to community images should be added here, built using
the GitHub workflow, and pushed to the appropriate location.

## Buildkit

All buildkit images have had their APKdependencies upgraded, and the rootless
image has been modified to expand the uid/gid map range and accommodate
environments where Istio is running.
