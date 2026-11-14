package bootstrap

import (
	"testing"
	"time"
)

// Модуль публикует маршруты только через публичную группу приложения,
// а не через отдельный провайдер сырого http.Handler.
func TestModuleRoutes(t *testing.T) {
	t.Parallel()
	m := mustModule(t, nil, nil, time.Second)
	routes := routesOf(m)
	if len(routes) != 1 {
		t.Fatalf("маршрутов модуля %d, ожидался 1", len(routes))
	}
	if routes[0].Prefix != "/" {
		t.Fatalf("префикс маршрута %q, ожидался %q", routes[0].Prefix, "/")
	}
	if routes[0].Handler == nil || routes[0].Handler != m.Handler() {
		t.Fatal("в маршруте не передан обработчик модуля")
	}
}
