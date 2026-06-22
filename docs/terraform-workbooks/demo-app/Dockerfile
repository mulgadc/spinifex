# Spinifex EKS demo app — multi-stage build to a tiny distroless image.
#
# Stage 1 compiles a static binary; stage 2 ships only that binary on a
# distroless base (no shell, no package manager) for a small, production-shaped
# image suitable for pushing to ECR and pulling onto EKS workers.

FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/spinifex-demo .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/spinifex-demo /spinifex-demo
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/spinifex-demo"]
