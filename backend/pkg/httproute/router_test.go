package httproute_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vendelev/sre-metrics/pkg/httproute"
)

func TestNewRouterMountsModuleRoutes(t *testing.T) {
	t.Parallel()
	routes := []httproute.Route{
		{
			Prefix: "/patient",
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprint(w, "patient:"+r.URL.Path)
			}),
		},
		{
			Prefix: "/login",
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprint(w, "login:"+r.URL.Path)
			}),
		},
	}
	handler, err := httproute.NewRouter(routes)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, want string }{
		{"/login/events", "login:/login/events"},
		{"/patient/events", "patient:/patient/events"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusOK || w.Body.String() != tc.want {
				t.Fatalf("%s: %d %q", tc.path, w.Code, w.Body.String())
			}
		})
	}
}

func TestNewRouterRejectsAmbiguousRoutes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		routes []httproute.Route
		want   string
	}{
		{"нет маршрутов", nil, "не заданы маршруты"},
		{
			"пустой префикс",
			[]httproute.Route{{Prefix: "", Handler: http.NotFoundHandler()}},
			"должен начинаться со слэша",
		},
		{
			"нет обработчика",
			[]httproute.Route{{Prefix: "/login", Handler: nil}},
			"не задан обработчик",
		},
		{
			"повтор префикса",
			[]httproute.Route{
				{Prefix: "/login", Handler: http.NotFoundHandler()},
				{Prefix: "/login", Handler: http.NotFoundHandler()},
			},
			"пересекается",
		},
		{
			"вложенный префикс",
			[]httproute.Route{
				{Prefix: "/login/extra", Handler: http.NotFoundHandler()},
				{Prefix: "/login", Handler: http.NotFoundHandler()},
			},
			"пересекается",
		},
		{
			"корневой префикс перекрывает модуль",
			[]httproute.Route{
				{Prefix: "/login", Handler: http.NotFoundHandler()},
				{Prefix: "/", Handler: http.NotFoundHandler()},
			},
			"пересекается",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := httproute.NewRouter(tc.routes)
			if err == nil {
				t.Fatal("конфликтная маршрутизация принята")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("неожиданная причина: %v", err)
			}
		})
	}
}
