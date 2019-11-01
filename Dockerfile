FROM alpine:3.9
LABEL maintainer="jan.knipper@sap.com"

RUN apk --no-cache add ca-certificates
COPY kubernetes-watchcache-exporter /kubernetes-watchcache-exporter
USER nobody:nobody

ENTRYPOINT ["/kubernetes-watchcache-exporter"]
CMD ["-logtostderr"]
