// Copyright 2016 Fraunhofer Institute for Applied Information Technology FIT

package data

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/farshidtz/senml"

	"net/http"

	paho "github.com/eclipse/paho.mqtt.golang"
	"github.com/linksmart/historical-datastore/common"
	"github.com/linksmart/historical-datastore/registry"
)

const (
	mqttRetryInterval = 10 // seconds
)

type MQTTConnector struct {
	sync.Mutex
	registry registry.Storage
	storage  Storage
	clientID string
	managers map[string]*Manager
	// cache of resource->ds
	cache map[string]*registry.DataStream
	// failed mqtt registrations
	failedRegistrations map[string]*registry.MQTTSource
}

type Manager struct {
	url    string
	client paho.Client
	// connector *MQTTConnector
	// total subscriptions for each topic in this manager
	subscriptions map[string]*Subscription
}

type Subscription struct {
	connector *MQTTConnector
	url       string
	topic     string
	qos       byte
	receivers int
}

func NewMQTTConnector(storage Storage, clientID string) (*MQTTConnector, error) {
	c := &MQTTConnector{
		storage:             storage,
		clientID:            clientID,
		managers:            make(map[string]*Manager),
		cache:               make(map[string]*registry.DataStream),
		failedRegistrations: make(map[string]*registry.MQTTSource),
	}
	return c, nil
}

func (c *MQTTConnector) Start(reg registry.Storage) error {
	c.registry = reg

	perPage := 100
	for page := 1; ; page++ {
		dataStreams, total, err := c.registry.GetMany(page, perPage)
		if err != nil {
			return fmt.Errorf("MQTT: Error getting data sources: %v", err)
		}

		for _, ds := range dataStreams {
			if ds.Source.SrcType == registry.MqttType {
				err := c.register(*ds.Source.MQTTSource)
				if err != nil {
					log.Printf("MQTT: Error registering subscription: %v. Retrying in %ds", err, mqttRetryInterval)
					c.failedRegistrations[ds.Name] = ds.Source.MQTTSource
				}
			}
		}

		if page*perPage >= total {
			break
		}
	}

	go c.retryRegistrations()

	return nil
}

func (c *MQTTConnector) flushCache() {
	c.cache = make(map[string]*registry.DataStream)
}

func (c *MQTTConnector) retryRegistrations() {
	for {
		time.Sleep(mqttRetryInterval * time.Second)
		c.Lock()
		for id, mqttSource := range c.failedRegistrations {
			err := c.register(*mqttSource)
			if err != nil {
				log.Printf("MQTT: Error registering subscription: %v. Retrying in %ds", err, mqttRetryInterval)
				continue
			}
			delete(c.failedRegistrations, id)
		}
		c.Unlock()
	}
}

func (c *MQTTConnector) register(source registry.MQTTSource) error {

	if _, exists := c.managers[source.BrokerURL]; !exists { // NO CLIENT FOR THIS BROKER
		manager := &Manager{
			url:           source.BrokerURL,
			subscriptions: make(map[string]*Subscription),
		}

		manager.subscriptions[source.Topic] = &Subscription{
			connector: c,
			url:       source.BrokerURL,
			topic:     source.Topic,
			qos:       source.QoS,
			receivers: 1,
		}

		opts := paho.NewClientOptions() // uses defaults: https://godoc.org/github.com/eclipse/paho.mqtt.golang#NewClientOptions
		opts.AddBroker(source.BrokerURL)
		opts.SetClientID(fmt.Sprintf("HDS-%s", c.clientID))
		opts.SetOnConnectHandler(manager.onConnectHandler)
		opts.SetConnectionLostHandler(manager.onConnectionLostHandler)
		if source.Username != "" {
			opts.SetUsername(source.Username)
			opts.SetPassword(source.Password)
		}
		// TODO: add support for certificate auth
		//
		manager.client = paho.NewClient(opts)

		if token := manager.client.Connect(); token.Wait() && token.Error() != nil {
			return fmt.Errorf("MQTT: Error connecting to broker %v: %v", source.BrokerURL, token.Error())
		}
		c.managers[source.BrokerURL] = manager

	} else { // THERE IS A CLIENT FOR THIS BROKER
		manager := c.managers[source.BrokerURL]

		// TODO: check if another wildcard subscription matches the topic.
		if _, exists := manager.subscriptions[source.Topic]; !exists { // NO SUBSCRIPTION FOR THIS TOPIC
			subscription := &Subscription{
				connector: c,
				url:       source.BrokerURL,
				topic:     source.Topic,
				qos:       source.QoS,
				receivers: 1,
			}
			// Subscribe
			if token := manager.client.Subscribe(subscription.topic, subscription.qos, subscription.onMessage); token.Wait() && token.Error() != nil {
				return fmt.Errorf("MQTT: Error subscribing: %v", token.Error())
			}
			manager.subscriptions[source.Topic] = subscription
			log.Printf("MQTT: %s: Subscribed to %s", source.BrokerURL, source.Topic)

		} else { // There is a subscription for this topic
			//log.Printf("MQTT: %s: Already subscribed to %s", mqttConf.BrokerURL, mqttConf.Topic)
			manager.subscriptions[source.Topic].receivers++
		}
	}

	return nil
}

func (c *MQTTConnector) unregister(mqttSource *registry.MQTTSource) error {
	manager := c.managers[mqttSource.BrokerURL]
	// There may be no subscriptions due to a failed registration when HDS is restarted
	if manager == nil {
		return nil
	}
	manager.subscriptions[mqttSource.Topic].receivers--

	if manager.subscriptions[mqttSource.Topic].receivers == 0 {
		// Unsubscribe
		if token := manager.client.Unsubscribe(mqttSource.Topic); token.Wait() && token.Error() != nil {
			return fmt.Errorf("MQTT: Error unsubscribing: %v", token.Error())
		}
		delete(manager.subscriptions, mqttSource.Topic)
		log.Printf("MQTT: %s: Unsubscribed from %s", mqttSource.BrokerURL, mqttSource.Topic)
	}
	if len(manager.subscriptions) == 0 {
		// Disconnect
		manager.client.Disconnect(250)
		delete(c.managers, mqttSource.BrokerURL)
		log.Printf("MQTT: %s: Disconnected!", mqttSource.BrokerURL)
	}

	return nil
}

func (m *Manager) onConnectHandler(client paho.Client) {
	log.Printf("MQTT: %s: Connected.", m.url)
	m.client = client
	for _, subscription := range m.subscriptions {
		if token := m.client.Subscribe(subscription.topic, subscription.qos, subscription.onMessage); token.Wait() && token.Error() != nil {
			log.Printf("MQTT: %s: Error subscribing: %v", m.url, token.Error())
		}
		log.Printf("MQTT: %s: Subscribed to %s", m.url, subscription.topic)
	}
}

func (m *Manager) onConnectionLostHandler(client paho.Client, err error) {
	log.Printf("MQTT: %s: Connection lost: %v", m.url, err)
}

func (s *Subscription) onMessage(client paho.Client, msg paho.Message) {
	t1 := time.Now()

	logHeader := fmt.Sprintf("\"SUB %s MQTT/QOS%d\"", msg.Topic(), msg.Qos())
	logMQTTError := func(code int, format string, v ...interface{}) {
		log.Printf("%s %d %v %s", logHeader, code, time.Now().Sub(t1), fmt.Sprintf(format, v...))
	}

	//log.Printf("MQTT: %s %s", msg.Topic(), msg.Payload())

	senmlPack, err := senml.Decode(msg.Payload(), senml.JSON)
	if err != nil {
		logMQTTError(http.StatusBadRequest, "Error parsing json: %s : %v", msg.Payload(), err)
		return
	}

	// Fill the data map with provided data points
	records := senmlPack.Normalize()
	data := make(map[string]senml.Pack)
	sources := make(map[string]*registry.DataStream)
	for _, r := range records {
		// Find the data source for this entry
		ds, exists := s.connector.cache[r.Name]
		if !exists {
			ds, err = s.connector.registry.Get(r.Name)
			if err != nil {
				if registry.ErrType(err, registry.ErrNotFound) {
					logMQTTError(http.StatusNotFound, "Warning: Resource not found: %v", r.Name)
					continue
				}
				logMQTTError(http.StatusInternalServerError, "Error finding resource: %v", r.Name)
				continue
			}

			s.connector.cache[r.Name] = ds
		}

		// Check if the message is wanted
		if ds.Source.MQTTSource == nil {
			logMQTTError(http.StatusNotAcceptable, "Ignoring unwanted message for resource: %v", r.Name)
			continue
		}
		if ds.Source.MQTTSource.BrokerURL != s.url {
			logMQTTError(http.StatusNotAcceptable, "Ignoring message from unwanted broker %v for data source: %v", s.url, r.Name)
			continue
		}
		if ds.Source.MQTTSource.Topic != s.topic {
			logMQTTError(http.StatusNotAcceptable, "Ignoring message with unwanted topic %v for data source: %v", s.topic, r.Name)
			continue
		}

		// Check if type of value matches the data source type in registry
		typeError := false
		switch ds.Type {
		case common.FLOAT:
			if r.Value == nil {
				typeError = true
			}
		case common.STRING:
			if r.StringValue == "" {
				typeError = true
			}
		case common.BOOL:
			if r.BoolValue == nil {
				typeError = true
			}
		}
		if typeError {
			logMQTTError(http.StatusBadRequest,
				"Value for %v is empty or has a type other than what is set in registry: %v", r.Name, ds.Type)
			continue
		}

		_, ok := data[ds.Name]
		if !ok {
			data[ds.Name] = []senml.Record{}
			sources[ds.Name] = ds
		}
		data[ds.Name] = append(data[ds.Name], r)
	}

	if len(data) > 0 {
		// Add data to the storage
		err = s.connector.storage.Submit(data, sources)
		if err != nil {
			logMQTTError(http.StatusInternalServerError, "Error writing data to the database: %v", err)
			return
		}

		log.Printf("%s %d %v\n", logHeader, http.StatusAccepted, time.Now().Sub(t1))
	}
}

// NOTIFICATION HANDLERS

// CreateHandler handles the creation of a new data source
func (c *MQTTConnector) CreateHandler(ds registry.DataStream) error {
	c.Lock()
	defer c.Unlock()

	if ds.Source.MQTTSource != nil {
		err := c.register(*ds.Source.MQTTSource)
		if err != nil {
			return fmt.Errorf("MQTT: Error adding subscription: %v", err)
		}
	}

	return nil
}

// UpdateHandler handles updates of a data source
func (c *MQTTConnector) UpdateHandler(oldDS registry.DataStream, newDS registry.DataStream) error {
	c.Lock()
	defer c.Unlock()

	if oldDS.Retention != newDS.Retention {
		c.flushCache()
	}

	if oldDS.Source.MQTTSource != newDS.Source.MQTTSource {
		// Remove old subscription
		if oldDS.Source.MQTTSource != nil {
			err := c.unregister(oldDS.Source.MQTTSource)
			if err != nil {
				return fmt.Errorf("MQTT: Error removing subscription: %v", err)
			}
		}
		delete(c.failedRegistrations, oldDS.Name)
		// Add new subscription
		if newDS.Source.MQTTSource != nil {
			err := c.register(*newDS.Source.MQTTSource)
			if err != nil {
				return fmt.Errorf("MQTT: Error adding subscription: %v", err)
			}
		}
	}
	return nil
}

// DeleteHandler handles deletion of a data source
func (c *MQTTConnector) DeleteHandler(oldDS registry.DataStream) error {
	c.Lock()
	defer c.Unlock()

	c.flushCache()

	// Remove old subscription
	if oldDS.Source.MQTTSource != nil {
		delete(c.cache, oldDS.Name)
		err := c.unregister(oldDS.Source.MQTTSource)
		if err != nil {
			return fmt.Errorf("MQTT: Error removing subscription: %v", err)
		}
		delete(c.failedRegistrations, oldDS.Name)
	}
	return nil
}
