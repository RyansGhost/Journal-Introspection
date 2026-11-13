# Build a static binary, then ship it alone in an empty image.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CMD=fakekev builds the stand-in grader instead (see compose.local.yaml).
ARG CMD=introspect
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/introspect ./cmd/$CMD \
 && mkdir -p /out/data

FROM scratch
COPY --from=build /out/introspect /introspect
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENV ORG_ROOT=/org DATA_DIR=/data ADDR=:8080
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s CMD ["/introspect", "healthcheck"]
ENTRYPOINT ["/introspect"]
CMD ["serve"]
