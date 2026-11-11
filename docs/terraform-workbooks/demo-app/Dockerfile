# Spinifex EKS demo app — multi-stage build to a tiny distroless image.
#
# Stage 1 compiles a static binary; stage 2 ships only that binary on a
# distroless base (no shell, no package manager) for a small, production-shaped
# image suitable for pushing to ECR and pulling onto EKS workers.

FROM golang:1.27.4-alpine@sha256:0a5f7fe796599ff487605987432e7619ae45beeb2fba4c2ce0b907879932aebf AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/spinifex-demo .

FROM gcr.io/distroless/static:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
COPY --from=build /out/spinifex-demo /spinifex-demo
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/spinifex-demo"]
