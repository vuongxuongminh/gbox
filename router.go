package gbox

import (
	"bytes"
	"errors"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/gbox-proxy/gbox/admin"
	"github.com/gbox-proxy/gbox/admin/generated"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/jensneuse/graphql-go-tools/pkg/graphql"
	"go.uber.org/zap"
)

const (
	adminPlaygroundPath = "/admin"
	adminGraphQLPath    = "/admin/graphql"
	playgroundPath      = "/"
	graphQLPath         = "/graphql"
)

func (h *Handler) initRouter() {
	router := mux.NewRouter()
	router.Path(graphQLPath).HeadersRegexp(
		"content-type", "application/json*",
	).Methods("POST").HandlerFunc(h.GraphQLHandle)
	router.Path(graphQLPath).HeadersRegexp(
		"upgrade", "^websocket$",
		"sec-websocket-protocol", "^graphql-(transport-)?ws$",
	).Methods("GET").HandlerFunc(h.GraphQLOverWebsocketHandle)

	if h.Caching != nil {
		router.Path(adminGraphQLPath).HeadersRegexp(
			"content-type", "application/json*",
		).Methods("POST").HandlerFunc(h.AdminGraphQLHandle)
	}

	if !h.DisabledPlaygrounds {
		ph := playground.Handler("GraphQL playground", graphQLPath)
		router.Path(playgroundPath).Methods("GET").Handler(ph)

		if h.Caching != nil {
			phAdmin := playground.Handler("Admin GraphQL playground", adminGraphQLPath)
			router.Path(adminPlaygroundPath).Methods("GET").Handler(phAdmin)
		}
	}

	if len(h.CORSOrigins) == 0 {
		h.router = router

		return
	}

	h.router = handlers.CORS(
		handlers.AllowCredentials(),
		handlers.AllowedOrigins(h.CORSOrigins),
		handlers.AllowedHeaders(h.CORSAllowedHeaders),
	)(router)
}

// GraphQLOverWebsocketHandle handling websocket connection between client & upstream.
func (h *Handler) GraphQLOverWebsocketHandle(w http.ResponseWriter, r *http.Request) {
	reporter := r.Context().Value(errorReporterCtxKey).(*errorReporter)

	if err := h.rewriteHandle(w, r); err != nil {
		reporter.error = err

		return
	}

	n := r.Context().Value(nextHandlerCtxKey).(caddyhttp.Handler)
	mr := newWebsocketMetricsResponseWriter(w, h.schema, h)
	reporter.error = h.ReverseProxy.ServeHTTP(mr, r, n)
}

// GraphQLHandle ensure GraphQL request is safe before forwarding to upstream and caching query result of it.
func (h *Handler) GraphQLHandle(w http.ResponseWriter, r *http.Request) {
	reporter := r.Context().Value(errorReporterCtxKey).(*errorReporter)

	if err := h.rewriteHandle(w, r); err != nil {
		reporter.error = err

		return
	}

	gqlRequest, err := h.unmarshalHTTPRequest(r)
	if err != nil {
		h.logger.Debug("unmarshal GQL cachingRequest from http cachingRequest failure", zap.Error(err))
		reporter.error = writeResponseErrors(err, w)

		return
	}

	isIntrospectQuery, _ := gqlRequest.IsIntrospectionQuery()
	start := time.Now()

	h.addMetricsBeginRequest(gqlRequest)
	defer func() {
		h.addMetricsEndRequest(gqlRequest, time.Since(start))
	}()

	if isIntrospectQuery && h.DisabledIntrospection {
		reporter.error = writeResponseErrors(errors.New("introspection queries are not allowed"), w)

		return
	}

	if h.Complexity != nil {
		requestErrors := h.Complexity.validateRequest(h.schema, gqlRequest)

		if requestErrors.Count() > 0 {
			reporter.error = writeResponseErrors(requestErrors, w)

			return
		}
	}

	n := r.Context().Value(nextHandlerCtxKey).(caddyhttp.Handler)

	if h.Caching != nil {
		cachingRequest := newCachingRequest(r, h.schemaDocument, h.schema, gqlRequest)
		reverse := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
			return h.ReverseProxy.ServeHTTP(w, r, n)
		})

		if err = h.Caching.HandleRequest(w, cachingRequest, reverse); err != nil {
			reporter.error = writeResponseErrors(err, w)

			return
		}

		return
	}

	reporter.error = h.ReverseProxy.ServeHTTP(w, r, n)
}

func (h *Handler) unmarshalHTTPRequest(r *http.Request) (*graphql.Request, error) {
	gqlRequest := new(graphql.Request)
	rawBody, _ := ioutil.ReadAll(r.Body)
	r.Body = ioutil.NopCloser(bytes.NewBuffer(rawBody))
	copyHTTPRequest, err := http.NewRequest(r.Method, r.URL.String(), ioutil.NopCloser(bytes.NewBuffer(rawBody))) // nolint:noctx
	if err != nil {
		return nil, err
	}

	if err = graphql.UnmarshalHttpRequest(copyHTTPRequest, gqlRequest); err != nil {
		return nil, err
	}

	if err = normalizeGraphqlRequest(h.schema, gqlRequest); err != nil {
		return nil, err
	}

	return gqlRequest, nil
}

// AdminGraphQLHandle purging query result cached and describe cache key.
func (h *Handler) AdminGraphQLHandle(w http.ResponseWriter, r *http.Request) {
	resolver := admin.NewResolver(h.schema, h.schemaDocument, h.logger, h.Caching)
	gqlGen := handler.NewDefaultServer(generated.NewExecutableSchema(generated.Config{Resolvers: resolver}))

	gqlGen.ServeHTTP(w, r)
}

func (h *Handler) rewriteHandle(w http.ResponseWriter, r *http.Request) error {
	n := caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
		return nil // trick for skip passing cachingRequest to next handle
	})

	return h.Rewrite.ServeHTTP(w, r, n)
}
