// Package bootstrap собирает HTTP-сервер без запуска ресурсов.
package bootstrap

import (
	"log"
	"net/http"

	pkgApi "github.com/vendelev/sre-metrics/pkg/httpserver/api"
	pkgConfig "github.com/vendelev/sre-metrics/pkg/httpserver/config"
	iHttpserver "github.com/vendelev/sre-metrics/pkg/httpserver/internal/infrastructure/http"
)

// New собирает сервер; открытие listener и запуск Serve принадлежат потребителю.
func New(cfg pkgConfig.HTTP, handler http.Handler, errorLog *log.Logger) pkgApi.Server {
	return iHttpserver.New(cfg, handler, errorLog)
}
