FROM golang:1.26-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
RUN CGO_ENABLED=0 go build -o server main.go

FROM alpine:3.23
# One source, two entries: the fiber-prefork entry builds this same image with
# FIBER_PREFORK=1, which is the only thing that separates the two. It stays off
# here, so this entry is Fiber in its default single-process configuration.
ARG FIBER_PREFORK=0
ENV FIBER_PREFORK=${FIBER_PREFORK}
COPY --from=build /app/server /server
EXPOSE 8080 8081
CMD ["/server"]
