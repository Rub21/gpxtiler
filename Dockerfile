FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/gpxtiler ./cmd/gpxtiler

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates lbzip2 \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/gpxtiler /usr/local/bin/gpxtiler
ENTRYPOINT ["gpxtiler"]
