FROM golang:1.27.3-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY service/user-login/ ./service/user-login/
RUN CGO_ENABLED=0 go build -trimpath -o /out/user-login ./service/user-login/cmd

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/user-login /user-login
USER 65532:65532
ENTRYPOINT ["/user-login"]
