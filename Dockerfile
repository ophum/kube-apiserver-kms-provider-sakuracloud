FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/kube-apiserver-kms-plugin-sakuracloud .

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/kube-apiserver-kms-plugin-sakuracloud /kube-apiserver-kms-plugin-sakuracloud
ENTRYPOINT ["/kube-apiserver-kms-plugin-sakuracloud"]
