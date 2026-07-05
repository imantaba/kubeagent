# Multi-stage build for the kubeagent daemon (kubeagent watch).
# The final image is distroless/static:nonroot — no shell, runs as UID 65532,
# matching the read-only securityContext in deploy/deployment.yaml.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags "-X main.version=${VERSION}" -o /kubeagent .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /kubeagent /kubeagent
USER 65532:65532
ENTRYPOINT ["/kubeagent"]
