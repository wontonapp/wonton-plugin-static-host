# web_storage 的提供方：只用 Go 标准库，静态链接，跑在 distroless 上（没有 shell，健康检查用程序自己的 -healthcheck）。
FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine@sha256:0a5f7fe796599ff487605987432e7619ae45beeb2fba4c2ce0b907879932aebf AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod *.go ./
RUN go test ./... && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags=-s -o /out/wonton-web-storage .

FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
COPY --from=build /out/wonton-web-storage /usr/local/bin/wonton-web-storage
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/wonton-web-storage"]
