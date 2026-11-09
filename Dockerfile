# Multi-stage build: console, then a static Go binary on distroless.
FROM node:24-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
COPY api/ /src/api/
RUN npm run build

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/initxy/plugrelay/internal/version.Version=${VERSION}" \
    -o /out/plugrelay ./cmd/plugrelay

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/plugrelay /usr/local/bin/plugrelay
VOLUME /data
ENV PLUGRELAY_DATA_DIR=/data
EXPOSE 8787
ENTRYPOINT ["plugrelay"]
CMD ["serve", "--config", "/data/relay.yaml"]
