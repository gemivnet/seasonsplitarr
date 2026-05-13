FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download || true
COPY . .
RUN CGO_ENABLED=0 go build -o /out/seasonsplitarr ./cmd/seasonsplitarr

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/seasonsplitarr /usr/local/bin/seasonsplitarr
EXPOSE 7474
ENTRYPOINT ["seasonsplitarr"]
