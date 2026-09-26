# Build from the repository root. Independent of the onboarding worker image.
FROM docker.io/library/golang:1.26-alpine AS build
WORKDIR /src
COPY workflows/go.mod workflows/go.sum ./
RUN go mod download
COPY workflows/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/connections ./cmd/connections

FROM docker.io/alpine/helm:3.18.4 AS helm
FROM docker.io/library/alpine:3.22
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 connections
COPY --from=helm /usr/bin/helm /usr/local/bin/helm
COPY --from=build /out/connections /connections
COPY deploy/helm/agent-harness-tenant/ /charts/tenant/
ENV HELM_CACHE_HOME=/tmp/helm/cache HELM_CONFIG_HOME=/tmp/helm/config HELM_DATA_HOME=/tmp/helm/data
USER 10001:10001
ENTRYPOINT ["/connections"]
