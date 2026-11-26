FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/tokenanalyse ./cmd/tokenanalyse

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/tokenanalyse /usr/local/bin/tokenanalyse
VOLUME ["/data"]
EXPOSE 8787
ENTRYPOINT ["tokenanalyse"]
CMD ["node", "--db", "/data/tokenanalyse.db", "--listen", "0.0.0.0:8787"]
