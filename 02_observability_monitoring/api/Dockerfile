FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
COPY main.go ./
RUN go mod tidy && CGO_ENABLED=0 go build -o /out/lab-api .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/lab-api /lab-api
EXPOSE 8080
ENTRYPOINT ["/lab-api"]
