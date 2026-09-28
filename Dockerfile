FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /checkout-api ./cmd/api

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /checkout-api /checkout-api
EXPOSE 8080
ENTRYPOINT ["/checkout-api"]

