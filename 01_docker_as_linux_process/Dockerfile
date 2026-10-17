FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -o /api .

FROM scratch
COPY --from=build /api /api
EXPOSE 8080
ENTRYPOINT ["/api"]
