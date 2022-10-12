## Test Cases:

- test caching
- test building
  - no cache
  - with cache
  - with exports
  - no exports
  - build args
  - concurrent
  - multi-stage
  - multi-tag
- test messaging
- test istio
- test eks, aks, gke


## Difficult Aspects:

- passing values into helmfile (including image tag)
- deciding when to run workflow
