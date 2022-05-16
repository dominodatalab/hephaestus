ARG BUILDKIT_TAG=master-rootless

FROM moby/buildkit:${BUILDKIT_TAG}
ARG ISTIO_GID=1337
USER root
RUN apk update && \
    apk upgrade && \
    rm -rf /var/cache/apk
RUN addgroup -S -g $ISTIO_GID istio && \
    addgroup user istio && \
    echo user:150000:150000 | tee /etc/subuid | tee /etc/subgid && \
    echo user:$ISTIO_GID:1 >> /etc/subgid
USER 1000:1000
