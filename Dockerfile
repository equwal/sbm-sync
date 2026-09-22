# Image of sbm-sync: the static binary on distroless, as user nonroot.
# compose.yaml builds it and puts Caddy in front for TLS.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go pages.html ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /sbm-sync . && mkdir /data

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /sbm-sync /usr/local/bin/sbm-sync
# A new volume on /data gets the owner of this directory: nonroot.
COPY --from=build --chown=65532:65532 /data /data
ENV SBM_ADDR=:8750 SBM_DATA=/data
EXPOSE 8750
ENTRYPOINT ["/usr/local/bin/sbm-sync"]
