FROM golang:1.27.3-alpine AS build
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
COPY backend/core/ ./core/
COPY backend/pkg/ ./pkg/
COPY backend/user-login/ ./user-login/
COPY backend/cmd/ ./cmd/
RUN CGO_ENABLED=0 go build -trimpath -o /out/user-login ./cmd/user-login-httpserver

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/user-login /user-login
USER 65532:65532
ENTRYPOINT ["/user-login"]
