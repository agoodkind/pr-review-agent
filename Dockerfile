ARG TARGETARCH

FROM gcr.io/distroless/static-debian12:nonroot@sha256:1b7b9f0f0e0a1d2155f531db587cc48ec26aaf97ab64364225f5bf18a054e66a

ARG SOURCE_REVISION
ARG TARGETARCH
ARG VERSION

LABEL org.opencontainers.image.source="https://github.com/agoodkind/pr-review-agent"
LABEL org.opencontainers.image.revision="${SOURCE_REVISION}"
LABEL org.opencontainers.image.version="${VERSION}"

COPY dist/pr-review-agent_linux_${TARGETARCH}/pr-review-agent /pr-review-agent

USER 65532:65532

EXPOSE 3000

ENTRYPOINT ["/pr-review-agent"]
