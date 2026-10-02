FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /emulator ./cmd/emulator

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /emulator /emulator
EXPOSE 8086
ENTRYPOINT ["/emulator", "-host", "0.0.0.0", "-port", "8086"]
