// Copyright 2016 Fraunhofer Institute for Applied Information Technology FIT

// Package data implements Data API
package data

import (
	"time"

	"code.linksmart.eu/hds/historical-datastore/registry"
	"github.com/cisco/senml"
)

// RecordSet describes the recordset returned on querying the Data API
type RecordSet struct {
	// URL is the URL of the returned recordset in the Data API
	URL string `json:"url"`
	// Data is a SenML object with data records, where
	// Name (bn and n) constitute the resource URL of the corresponding Data Source(s)
	Data []senml.SenMLRecord `json:"data"`
	// Time is the time of query in milliseconds
	Time float64 `json:"time"`
	// Page is the current page in Data pagination
	Page int `json:"page"`
	// PerPage is the results per page in Data pagination
	PerPage int `json:"per_page"`
	// Total is the total #of pages in Data pagination
	Total int `json:"total"`
}

type Query struct {
	Start time.Time
	End   time.Time
	Sort  string
	Limit int
}

// Storage is an interface of a Data storage backend
type Storage interface {
	// Adds data points for multiple data sources
	// data is a map where keys are data source ids
	// sources is a map where keys are data source ids
	Submit(data map[string][]senml.SenMLRecord, sources map[string]*registry.DataSource) error

	// Queries data for specified data sources
	Query(q Query, page, perPage int, sources ...*registry.DataSource) (senml.SenML, int, error)

	// EventListener includes methods for event handling
	registry.EventListener
}

// Supported content-types for data ingestion
var SupportedContentTypes = map[string]bool{
	"application/senml+json": true,
}
