FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /emulator ./cmd/emulator

# cbt has no tagged releases, so this pins a pseudo-version.
FROM golang:1.26 AS cbt-build
RUN CGO_ENABLED=0 GOBIN=/out go install -trimpath cloud.google.com/go/cbt@v0.0.0-20260929161620-d553ae611d4e

FROM debian:12-slim AS init
COPY --from=cbt-build /out/cbt /usr/local/bin/cbt
CMD ["bash"]

# The last stage is the default target.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /emulator /emulator
EXPOSE 8086
ENTRYPOINT ["/emulator", "-host", "0.0.0.0", "-port", "8086"]
HEALTHCHECK --interval=2s --retries=15 CMD ["/emulator", "-probe", "localhost:8086"]
