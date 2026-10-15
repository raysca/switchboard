FROM golang:1.27.1-bookworm@sha256:69a7b9788769bec032d238959b61854e9ae87f57be9029ec04e9885fabf99195 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/offline-adapter ./cmd/offline-adapter

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/offline-adapter /offline-adapter
EXPOSE 8099
ENTRYPOINT ["/offline-adapter"]
