# Scheduler API image (M6). The service is a single static binary talking to
# a SQLite file; the container carries no state. The database path must be a
# mounted volume (DATABASE_PATH=/data/scheduler.db).
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/scheduler-api ./cmd/api

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/scheduler-api /scheduler-api
# distroless static ships the CA bundle needed for the HTTPS calls the
# adapters make; nothing else is installed on purpose.
EXPOSE 8080
ENTRYPOINT ["/scheduler-api"]
