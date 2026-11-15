package httproute

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"
)

// NewRouter собирает единый обработчик приложения из публичных маршрутов
// модулей. Сортировка делает порядок монтирования детерминированным
// независимо от порядка получения маршрутов в composition root.
// Пересечение префиксов делает маршрутизацию неоднозначной и отклоняется.
func NewRouter(routes []Route) (http.Handler, error) {
	ordered := slices.Clone(routes)
	slices.SortFunc(ordered, func(a, b Route) int {
		return strings.Compare(a.Prefix, b.Prefix)
	})
	if len(ordered) == 0 {
		return nil, errors.New("не заданы маршруты модулей")
	}

	router := chi.NewRouter()
	mounted := make([]string, 0, len(ordered))
	for _, route := range ordered {
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
func validateRoute(route Route) error {
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
