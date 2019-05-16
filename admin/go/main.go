package main

import (
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/rs/zerolog/hlog"
	"github.com/rs/zerolog/log"
	"gitlab.corp.mail.ru/mydev/onlineconf/admin/go/admin"
	. "gitlab.corp.mail.ru/mydev/onlineconf/admin/go/common"
	"gitlab.corp.mail.ru/mydev/onlineconf/admin/go/resolver"
)

func main() {
	r := mux.NewRouter()

	registerPProf(r.PathPrefix("/debug/pprof/").Subrouter())

	resolverRouter := r.PathPrefix("/client/").Subrouter()
	resolverRouter.Use(resolver.AuthMiddleware)
	resolver.RegisterRoutes(resolverRouter)

	adminRouter := r.PathPrefix("/").Subrouter()
	adminRouter.Use(admin.AuthMiddleware)
	admin.RegisterRoutes(adminRouter)

	handler := hlog.AccessHandler(writeAccessLog)(r)
	if AdminConfig.HTTP.BehindReverseProxy {
		handler = handlers.ProxyHeaders(handler)
	}
	handler = hlog.RequestIDHandler("reqid", "")(handler)
	handler = hlog.NewHandler(log.Logger)(handler)

	server := http.Server{
		Addr:    AdminConfig.HTTP.Listen,
		Handler: handler,
	}
	if err := server.ListenAndServe(); err != nil {
		log.Fatal().Err(err).Msg("failed to start HTTP server")
	}
}

func writeAccessLog(r *http.Request, status, size int, duration time.Duration) {
	l := hlog.FromRequest(r).Info().
		Str("remote", r.RemoteAddr)
	if r.URL.User != nil {
		l = l.Str("username", r.URL.User.Username())
	}
	l.Str("method", r.Method).
		Str("uri", r.RequestURI).
		Int("status", status).
		Int("size", size).
		Dur("duration", duration).
		Msg("")
}

func registerPProf(r *mux.Router) {
	r.Use(admin.RootUsersOnly)

	r.Path("/cmdline").HandlerFunc(pprof.Cmdline)
	r.Path("/profile").HandlerFunc(pprof.Profile)
	r.Path("/symbol").HandlerFunc(pprof.Symbol)
	r.Path("/trace").HandlerFunc(pprof.Trace)
	r.PathPrefix("/").HandlerFunc(pprof.Index)
}
