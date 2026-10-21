# Spinifex EKS demo app — multi-stage build to a tiny distroless image.
#
# Stage 1 compiles a static binary; stage 2 ships only that binary on a
# distroless base (no shell, no package manager) for a small, production-shaped
# image suitable for pushing to ECR and pulling onto EKS workers.

FROM golang:1.27.3-alpine@sha256:f2c872bcee949b7be9c490efd02b48a8a2fbf947cdf533f1c58525ec9d8170fd AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/spinifex-demo .

FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --from=build /out/spinifex-demo /spinifex-demo
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/spinifex-demo"]
