package main

import (
	"expvar"
	"net/http"

	"github.com/julienschmidt/httprouter"
)

func routes() http.Handler {
	router := httprouter.New()

	router.NotFound = http.HandlerFunc(receiveHandler)
	router.MethodNotAllowed = http.HandlerFunc(methodNotAllowedResponse)

	router.HandlerFunc(http.MethodGet, "/v1/healthcheck", healthcheckHandler)
	router.Handler(http.MethodGet, "/debug/vars", expvar.Handler())

	return metrics(recoverPanic(rateLimit(router)))
}
