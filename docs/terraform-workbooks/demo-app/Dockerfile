# Spinifex demo app for EKS — multi-stage build to a tiny distroless image.
#
# Stage 1 compiles a static binary; stage 2 ships only that binary on a
# distroless base (no shell, no package manager) for a small, production-shaped
# image suitable for pushing to ECR and pulling onto EKS workers.

FROM golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/spinifex-demo .

FROM gcr.io/distroless/static:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
COPY --from=build /out/spinifex-demo /spinifex-demo
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/spinifex-demo"]
