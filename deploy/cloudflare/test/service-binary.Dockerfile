FROM scratch
COPY pr-review-agent /pr-review-agent
COPY runtime.json /runtime.json
COPY config /config
USER 65532:65532
ENTRYPOINT ["/pr-review-agent"]
