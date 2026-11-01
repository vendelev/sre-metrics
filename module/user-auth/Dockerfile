FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY module/user-auth/ ./module/user-auth/
RUN CGO_ENABLED=0 go build -trimpath -o /out/user-auth ./module/user-auth/cmd/user-auth

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/user-auth /user-auth
USER 65532:65532
ENTRYPOINT ["/user-auth"]
