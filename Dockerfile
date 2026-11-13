FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/rlscheck ./cmd/rlscheck

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/api /out/rlscheck /usr/local/bin/
ENV GIN_MODE=release APP_ENV=production PORT=8080
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/usr/local/bin/api"]
