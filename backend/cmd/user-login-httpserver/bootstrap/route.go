package bootstrap

import (
	"net/http"

	"go.uber.org/dig"

	"github.com/vendelev/sre-metrics/pkg/httproute"
)

type rootHandlerIn struct {
	dig.In

	Routes []httproute.Route `group:"http-routes"`
}

// rootHandler адаптирует группу маршрутов DI к generic-конструктору
func rootHandler(in rootHandlerIn) (http.Handler, error) {
	return httproute.NewRouter(in.Routes)
}
