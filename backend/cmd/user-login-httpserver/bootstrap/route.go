package bootstrap

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"go.uber.org/dig"

	"github.com/go-chi/chi/v5"

	"github.com/vendelev/sre-metrics/platform/web"
)

type rootHandlerIn struct {
	dig.In

	Routes []web.Route `group:"http-routes"`
}

// rootHandler собирает единый обработчик приложения из публичных маршрутов
// модулей. core получает только этот результат и не знает модули.
// Сортировка делает порядок монтирования детерминированным независимо
// от порядка регистрации провайдеров DI.
func rootHandler(in rootHandlerIn) (http.Handler, error) {
	routes := slices.Clone(in.Routes)
	slices.SortFunc(routes, func(a, b web.Route) int {
		return strings.Compare(a.Prefix, b.Prefix)
	})
	if len(routes) == 0 {
		return nil, errors.New("не заданы маршруты модулей")
	}

	router := chi.NewRouter()
	mounted := make([]string, 0, len(routes))
	for _, route := range routes {
		if err := validateRoute(route); err != nil {
			return nil, err
		}
		if conflict := routeConflict(route.Prefix, mounted); conflict != "" {
			return nil, fmt.Errorf("префикс маршрута %q пересекается с %q", route.Prefix, conflict)
		}
		mounted = append(mounted, route.Prefix)
		router.Mount(route.Prefix, route.Handler)
	}
	return router, nil
}

// validateRoute защищает сборку приложения от неполного дескриптора.
func validateRoute(route web.Route) error {
	if !strings.HasPrefix(route.Prefix, "/") {
		return fmt.Errorf("префикс маршрута %q должен начинаться со слэша", route.Prefix)
	}
	if route.Handler == nil {
		return fmt.Errorf("не задан обработчик модуля для префикса %q", route.Prefix)
	}
	return nil
}

// routeConflict возвращает уже смонтированный префикс, с которым пересекается
// новый. Корневой префикс перекрывает любой другой; точное совпадение и вложенные
// префиксы тоже делают маршрутизацию неоднозначной.
func routeConflict(prefix string, mounted []string) string {
	for _, existing := range mounted {
		if prefix == existing || prefix == "/" || existing == "/" ||
			strings.HasPrefix(prefix, existing+"/") ||
			strings.HasPrefix(existing, prefix+"/") {
			return existing
		}
	}
	return ""
}
