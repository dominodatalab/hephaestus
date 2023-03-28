ARG BUILDKIT_TAG

FROM moby/buildkit:${BUILDKIT_TAG} AS root
RUN apk update && \
    apk upgrade && \
    rm -rf /var/cache/apk

FROM moby/buildkit:${BUILDKIT_TAG} AS rootless
ARG ISTIO_GID=1337
ARG UID=1000
USER root
RUN apk update && \
    apk upgrade && \
    rm -rf /var/cache/apk

RUN deluser user || echo this fails for no reason && \
    adduser -u $UID -D user && \
    chown -R user:user /home/user && \
    mkdir -p /run/user/$UID && \
    chown user:root /run/user/$UID

ENV XDG_RUNTIME_DIR=/run/user/$UID
ENV BUILDKIT_HOST=unix:///run/user/$UID/buildkit/buildkitd.sock

RUN addgroup -S -g $ISTIO_GID istio && \
    addgroup user istio && \
    echo user:100000:150000 | tee /etc/subuid | tee /etc/subgid && \
    echo user:$ISTIO_GID:1 >> /etc/subgid
USER $UID:$UID
