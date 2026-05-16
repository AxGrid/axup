FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS build
WORKDIR /src
ARG VERSION=dev
COPY go.mod ./
RUN go mod download
COPY main.go ./
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.Version=$VERSION" -o /out/echoapp ./...

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/echoapp /usr/local/bin/echoapp
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/echoapp"]
