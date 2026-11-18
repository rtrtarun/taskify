FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/taskify ./cmd/taskify \
 && CGO_ENABLED=0 go build -trimpath -o /out/mock-agentpulse ./cmd/mock-agentpulse

FROM alpine:3.20
RUN adduser -D -u 10001 taskify
COPY --from=build /out/taskify /out/mock-agentpulse /usr/local/bin/
USER taskify
EXPOSE 8080
ENTRYPOINT ["taskify"]
