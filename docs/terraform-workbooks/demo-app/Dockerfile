# Spinifex EKS demo app — multi-stage build to a tiny distroless image.
#
# Stage 1 compiles a static binary; stage 2 ships only that binary on a
# distroless base (no shell, no package manager) for a small, production-shaped
# image suitable for pushing to ECR and pulling onto EKS workers.

FROM golang:1.27.3-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/spinifex-demo .

FROM gcr.io/distroless/static:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
COPY --from=build /out/spinifex-demo /spinifex-demo
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/spinifex-demo"]
