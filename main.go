// Copyright 2016 Fraunhofer Institute for Applied Information Technology FIT

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"

	_ "code.linksmart.eu/com/go-sec/auth/keycloak/validator"
	"code.linksmart.eu/com/go-sec/auth/validator"
	"code.linksmart.eu/hds/historical-datastore/aggregation"
	"code.linksmart.eu/hds/historical-datastore/common"
	"code.linksmart.eu/hds/historical-datastore/data"
	"code.linksmart.eu/hds/historical-datastore/registry"
	"github.com/gorilla/context"
	"github.com/justinas/alice"
)

const LINKSMART = `
╦   ╦ ╔╗╔ ╦╔═  ╔═╗ ╔╦╗ ╔═╗ ╦═╗ ╔╦╗ R
║   ║ ║║║ ╠╩╗  ╚═╗ ║║║ ╠═╣ ╠╦╝  ║
╩═╝ ╩ ╝╚╝ ╩ ╩  ╚═╝ ╩ ╩ ╩ ╩ ╩╚═  ╩
`

var (
	confPath    = flag.String("conf", "conf/historical-datastore.json", "Historical Datastore configuration file path")
	profile     = flag.Bool("profile", false, "Enable the HTTP server for runtime profiling")
	version     = flag.Bool("version", false, "Show the Historical Datastore API version")
	Version     = "N/A" // set with build flags
	BuildNumber = "N/A" // set with build flags
)

func main() {
	flag.Parse()
	if *version {
		fmt.Println(Version)
		return
	}
	fmt.Print(LINKSMART)
	logger.Printf("Starting Historical Datastore")
	logger.Printf("Version: %s", Version)
	logger.Printf("Build Number: %s", BuildNumber)
	common.APIVersion = Version

	if *profile {
		logger.Println("Starting runtime profiling server")
		go func() { logger.Println(http.ListenAndServe("0.0.0.0:6060", nil)) }()
	}

	// Load Config File
	conf, err := common.LoadConfig(confPath)
	if err != nil {
		logger.Fatalf("Config File: %s\n", err)
	}

	// registry
	var (
		regStorage registry.Storage
		regPubCh   *chan common.Notification
		closeReg   func() error
	)
	switch conf.Reg.Backend.Type {
	case "memory":
		regStorage, regPubCh = registry.NewMemoryStorage(conf.Reg)
	case "leveldb":
		regStorage, regPubCh, closeReg, err = registry.NewLevelDBStorage(conf.Reg, nil)
		if err != nil {
			logger.Fatalf("Failed to start LevelDB: %s\n", err)
		}
	}

	regAPI := registry.NewHTTPAPI(regStorage)
	registryClient := registry.NewLocalClient(regStorage)

	// data and aggregation backends
	var (
		dataStorage          data.Storage
		aggrStorage          aggregation.Storage
		dataSubCh, aggrSubCh chan<- common.Notification
	)
	switch conf.Data.Backend.Type {
	case "mongodb":
		logger.Fatalln("Mongodb is not supported after HDS v0.5.3")
	case "influxdb":
		dataStorage, dataSubCh, err = data.NewInfluxStorage(conf.Data, conf.Reg.RetentionPeriods)
		if err != nil {
			logger.Fatalf("Error creating influx storage: %v", err)
		}
		aggrStorage, aggrSubCh, err = aggregation.NewInfluxAggr(dataStorage.(*data.InfluxStorage))
		if err != nil {
			logger.Fatalf("Error creating influx aggr: %v", err)
		}
	}

	// TODO: disconnect on shutdown
	mqttSubCh, err := data.StartMQTTConnector(registryClient, dataStorage)
	if err != nil {
		logger.Fatalf("Error starting MQTT Connector: %v", err)
	}
	dataAPI := data.NewAPI(registryClient, dataStorage, conf.Data.AutoRegistration)

	// aggregation
	aggrAPI := aggregation.NewAPI(registryClient, aggrStorage)

	// Start the notifier
	common.StartNotifier(regPubCh, dataSubCh, aggrSubCh, mqttSubCh)

	commonHandlers := alice.New(
		context.ClearHandler,
		loggingHandler,
		recoverHandler,
		commonHeaders,
	)

	// http api
	router := newRouter()
	// generic handlers
	router.get("/health", commonHandlers.ThenFunc(healthHandler))
	router.options("/{path:.*}", commonHandlers.ThenFunc(optionsHandler))

	// Append auth handler if enabled
	if conf.Auth.Enabled {
		// Setup ticket validator
		v, err := validator.Setup(
			conf.Auth.Provider,
			conf.Auth.ProviderURL,
			conf.Auth.ServiceID,
			conf.Auth.BasicEnabled,
			conf.Auth.Authz)
		if err != nil {
			logger.Fatalf(err.Error())
		}

		commonHandlers = commonHandlers.Append(v.Handler)
	}

	// api root
	router.get("/", commonHandlers.ThenFunc(indexHandler))

	// registry api
	router.get("/registry", commonHandlers.ThenFunc(regAPI.Index))
	router.post("/registry", commonHandlers.ThenFunc(regAPI.Create))
	router.get("/registry/{id}", commonHandlers.ThenFunc(regAPI.Retrieve))
	router.put("/registry/{id}", commonHandlers.ThenFunc(regAPI.Update))
	router.delete("/registry/{id}", commonHandlers.ThenFunc(regAPI.Delete))
	router.get("/registry/{type}/{path}/{op}/{value:.*}", commonHandlers.ThenFunc(regAPI.Filter))

	// data api
	router.post("/data", commonHandlers.ThenFunc(dataAPI.SubmitWithoutID))
	router.post("/data/{id}", commonHandlers.ThenFunc(dataAPI.Submit))
	router.get("/data/{id}", commonHandlers.ThenFunc(dataAPI.Query))

	// aggregation api
	router.get("/aggr", commonHandlers.ThenFunc(aggrAPI.Index))
	router.get("/aggr/{path}/{op}/{value:.*}", commonHandlers.ThenFunc(aggrAPI.Filter))
	router.get("/aggr/{aggrid}/{uuid}", commonHandlers.ThenFunc(aggrAPI.Query))

	// Register in the service catalog(s)
	unregisterService := registerInServiceCatalog(conf)

	// Ctrl+C / Kill handling
	handler := make(chan os.Signal, 1)
	signal.Notify(handler, os.Interrupt, os.Kill)
	go func() {
		<-handler
		logger.Println("Shutting down...")

		// Unregister from the service catalog(s)
		unregisterService()

		// Close the Registry Storage
		if closeReg != nil {
			err := closeReg()
			if err != nil {
				logger.Println(err.Error())
			}
		}

		logger.Println("Stopped.")
		os.Exit(0)
	}()

	// Serve static web directory
	go webServer(conf)

	// start http server
	logger.Printf("Listening on %s:%d", conf.HTTP.BindAddr, conf.HTTP.BindPort)
	err = http.ListenAndServe(fmt.Sprintf("%s:%d", conf.HTTP.BindAddr, conf.HTTP.BindPort), router)
	if err != nil {
		logger.Fatalln(err)
	}
}

func webServer(conf *common.Config) {
	staticConf := map[string]interface{}{
		"apiPort": conf.HTTP.BindPort,
	}

	if conf.Auth.Enabled {
		staticConf["authEnabled"] = conf.Auth.Enabled
		staticConf["authProvider"] = conf.Auth.Provider
		staticConf["authProviderURL"] = conf.Auth.ProviderURL
		staticConf["authServiceID"] = conf.Auth.ServiceID
	}

	b, err := json.Marshal(staticConf)
	if err != nil {
		logger.Fatalln("Error marshalling web config file:", err)
	}

	err = os.MkdirAll(conf.Web.StaticDir+"/conf", 0755)
	if err != nil {
		logger.Fatalln("Error writing web config file:", err)
	}

	err = ioutil.WriteFile(conf.Web.StaticDir+"/conf/autogen_config.json", b, 0644)
	if err != nil {
		logger.Fatalln("Error writing web config file:", err)
	}

	mux := http.NewServeMux()
	fs := http.FileServer(http.Dir(conf.Web.StaticDir))
	mux.Handle("/", fs)
	go func() {
		err := http.ListenAndServe(fmt.Sprintf("%s:%d", conf.Web.BindAddr, conf.Web.BindPort), mux)
		if err != nil {
			logger.Fatalln(err)
		}
	}()
}
