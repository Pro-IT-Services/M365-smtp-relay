# Image for running a released Graph Relay binary. Built on the Docker host by docker/setup.sh and
# docker/update.sh from the binary in the release archive, so no registry and no Go are needed.
#
#   docker build -t graphrelay:v1.2.3 -f docker/Dockerfile .      (in the extracted archive)
FROM alpine:3

# CA certificates for the HTTPS calls to Microsoft Graph, Cloudflare and Let's Encrypt.
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -g 10001 graphrelay \
 && adduser -D -H -u 10001 -G graphrelay graphrelay

COPY graphrelay /usr/local/bin/graphrelay

# Unprivileged: the relay listens on high ports inside the container and Docker publishes them
# on 25/465/587 (see compose.yaml).
USER 10001:10001
WORKDIR /var/lib/graphrelay
VOLUME ["/var/lib/graphrelay"]
EXPOSE 2525 4465 5587 8443

ENTRYPOINT ["/usr/local/bin/graphrelay"]
CMD ["-config", "/etc/graphrelay/config.yaml"]
