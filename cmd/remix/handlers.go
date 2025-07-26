package main

import (
	"net/http"

	"github.com/sqlpipe/remix/internal/helpers"
	"github.com/sqlpipe/remix/internal/systems"
	"github.com/sqlpipe/remix/internal/vcs"
)

func receiveHandler(w http.ResponseWriter, r *http.Request) {

	path := r.URL.Path
	if len(path) > 0 && path[0] == '/' {
		path = path[1:]
	}

	systems.SystemMap[path].HandleWebhook(w, r)
}

func healthcheckHandler(w http.ResponseWriter, r *http.Request) {
	env := helpers.Envelope{
		"status": "available",
		"system_info": map[string]string{
			"version": vcs.Version(),
		},
	}

	err := helpers.WriteJSON(w, http.StatusOK, env, nil)
	if err != nil {
		serverErrorResponse(w, r, err)
	}
}
