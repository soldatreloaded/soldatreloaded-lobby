# The lobby: a static binary on distroless, run as nobody.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /lobby .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /lobby /lobby
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/lobby"]
CMD ["-listen", ":8080"]
