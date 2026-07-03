FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /luncur ./cmd/luncur

FROM alpine:3.21
# git: required by the git-push receiver (git-receive-pack, git archive).
RUN apk add --no-cache git ca-certificates
COPY --from=build /luncur /usr/local/bin/luncur
ENTRYPOINT ["/usr/local/bin/luncur"]
CMD ["serve"]
