package main

import (
	"flag"
	"net/http"

	"linksmart.eu/services/historical-datastore/Godeps/_workspace/src/github.com/gorilla/context"
	"linksmart.eu/services/historical-datastore/Godeps/_workspace/src/github.com/justinas/alice"
	"linksmart.eu/services/historical-datastore/registry"
)

func main() {
	var addr = flag.String("addr", ":8080", "HTTP bind address")

	flag.Parse()

	commonHandlers := alice.New(
		context.ClearHandler,
		loggingHandler,
		recoverHandler,
	)

	router := newRouter()

	// generic handlers
	router.get("/health", commonHandlers.ThenFunc(healthHandler))
	router.get("/", commonHandlers.ThenFunc(indexHandler))

	// registry api
	regAPI := registry.NewRegistryAPI( /* no configurations */ )
	router.get("/registry", commonHandlers.ThenFunc(regAPI.Index))
	router.post("/registry/", commonHandlers.ThenFunc(regAPI.Create))
	router.get("/registry/{id}", commonHandlers.ThenFunc(regAPI.Retrieve))
	router.put("/registry/{id}", commonHandlers.ThenFunc(regAPI.Update))
	router.delete("/registry/{id}", commonHandlers.ThenFunc(regAPI.Delete))
	router.get("/registry/{path}/{type}/{op}/{value}", commonHandlers.ThenFunc(regAPI.Filter))

	// data api

	// aggregation api

	// start http server
	http.ListenAndServe(*addr, router)
}
