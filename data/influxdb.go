// Copyright 2016 Fraunhofer Institute for Applied Information Technology FIT

package data

import (
	"encoding/json"
	"fmt"
	"github.com/cisco/senml"
	"math"
	"net/url"
	"strings"
	"sync"
	"time"

	"code.linksmart.eu/hds/historical-datastore/common"
	"code.linksmart.eu/hds/historical-datastore/registry"
	influx "github.com/influxdata/influxdb/client/v2"
	"github.com/influxdata/influxdb/models"
	"github.com/influxdata/influxql"
	"github.com/satori/go.uuid"
)

const influxPingTimeout = 30 * time.Second

// InfluxStorage implements a InfluxDB storage client for HDS Data API
type InfluxStorage struct {
	client           influx.Client
	config           influxStorageConfig
	retentionPeriods []string
	prepare          sync.WaitGroup
}

// NewInfluxStorage returns a new InfluxStorage
func NewInfluxStorage(conf common.DataConf, retentionPeriods []string) (*InfluxStorage, chan<- common.Notification, error) {
	cfg, err := initInfluxConf(conf.Backend.DSN)
	if err != nil {
		return nil, nil, logger.Errorf("Influx config error: %s", err)
	}
	cfg.replication = 1

	c, err := influx.NewHTTPClient(influx.HTTPConfig{
		Addr:     cfg.dsn,
		Username: cfg.username,
		Password: cfg.password,
	})
	if err != nil {
		return nil, nil, logger.Errorf("Error initializing influxdb client: %s", err)
	}

	s := &InfluxStorage{
		client:           c,
		config:           *cfg,
		retentionPeriods: retentionPeriods,
	}

	s.prepare.Add(1)
	go s.prepareStorage()

	// Run the notification listener
	ntChan := make(chan common.Notification)
	go NtfListener(s, ntChan)

	return s, ntChan, nil
}

// Submit adds multiple data points for multiple data sources
// data is a map where keys are data source ids
func (s *InfluxStorage) Submit(data map[string][]senml.SenMLRecord, sources map[string]*registry.DataSource) error {
	for id, dps := range data {
		bpConf := influx.BatchPointsConfig{
			Database:        s.config.database,
			Precision:       "us", // float64 can keep unix seconds at most with 7 significant digits: not enough for ns
			RetentionPolicy: s.RetentionPolicyName(sources[id].Retention),
		}
		logger.Debugf("Influx: %+v", bpConf)

		bp, err := influx.NewBatchPoints(bpConf)
		if err != nil {
			return logger.Errorf("Error creating batch points: %s", err)
		}
		for _, dp := range dps {
			var (
				tags   map[string]string
				fields map[string]interface{}
			)
			// tags
			tags = make(map[string]string)
			tags["name"] = dp.Name // must be the same as sources[id].Resource
			//tags["id"] = sources[id].ID
			if dp.Unit != "" {
				tags["units"] = dp.Unit
			}

			// fields
			fields = make(map[string]interface{})
			// The "value", "stringValue", and "booleanValue" fields MUST NOT appear together.
			if dp.Value != nil {
				fields["value"] = *dp.Value
			} else if dp.StringValue != "" {
				fields["stringValue"] = dp.StringValue
			} else if dp.BoolValue != nil {
				fields["booleanValue"] = *dp.BoolValue
			}

			// timestamp
			sec, frac := math.Modf(dp.Time)

			pt, err := influx.NewPoint(
				s.MeasurementName(id),
				tags,
				fields,
				time.Unix(int64(sec), int64(frac*(1e9))),
			)
			if err != nil {
				return logger.Errorf("Error creating data point for source %v: %s", sources[id].ID, err)
			}
			bp.AddPoint(pt)
		}
		err = s.client.Write(bp)
		if err != nil {
			var influxResponse influx.Response
			marshalErr := json.Unmarshal([]byte(err.Error()), &influxResponse)
			if marshalErr != nil {
				return logger.Errorf("Error writing: %s: %s", marshalErr, err)
			}
			if strings.Contains(influxResponse.Err, "partial write: points beyond retention policy dropped") {
				// TODO: send this to the client?
				logger.Println(influxResponse.Err)
				return nil
			}
			return logger.Errorf("Error writing: %s", influxResponse.Err)
		}
	}
	return nil
}

// Query retrieves data for specified data sources
func (s *InfluxStorage) Query(q Query, page, perPage int, sources ...*registry.DataSource) (senml.SenML, int, error) {
	total := 0

	// Set minimum time to 1970-01-01T00:00:00Z
	if q.Start.Before(time.Unix(0, 0)) {
		q.Start = time.Unix(0, 0)
		if q.End.Before(time.Unix(0, 1)) {
			return senml.SenML{}, 0, logger.Errorf("%s argument must be greater than 1970-01-01T00:00:00Z", common.ParamEnd)
		}
	}

	// If q.End is not set, make the query open-ended
	var timeCond string
	if q.Start.Before(q.End) {
		timeCond = fmt.Sprintf("time > '%s' AND time < '%s'", q.Start.Format(time.RFC3339), q.End.Format(time.RFC3339))
	} else {
		timeCond = fmt.Sprintf("time > '%s'", q.Start.Format(time.RFC3339))
	}

	perItems, offsets := common.PerItemPagination(q.Limit, page, perPage, len(sources))

	// Initialize sort order
	sort := "DESC"
	if q.Sort == common.ASC {
		sort = "ASC"
	}

	pack := senml.SenML{}
	pack.Records = make([]senml.SenMLRecord, 0)

	for i, ds := range sources {
		// Count total
		count, err := s.CountSprintf("SELECT COUNT(%s) FROM %s WHERE %s",
			s.FieldForType(ds.Type), s.MeasurementNameFQ(ds.Retention, s.MeasurementName(ds.ID)), timeCond)
		if err != nil {
			return senml.SenML{}, 0, logger.Errorf("Error counting records for source %v: %s", ds.Resource, err)
		}
		if count < 1 {
			//logger.Printf("There is no data for source %v", ds.Resource)
			continue
		}
		total += int(count)

		res, err := s.QuerySprintf("SELECT * FROM %s WHERE %s ORDER BY time %s LIMIT %d OFFSET %d",
			s.MeasurementNameFQ(ds.Retention, s.MeasurementName(ds.ID)), timeCond, sort, perItems[i], offsets[i])
		if err != nil {
			return senml.SenML{}, 0, logger.Errorf("Error retrieving a data point for source %v: %s", ds.Resource, err)
		}

		if len(res[0].Series) > 1 {
			return senml.SenML{}, 0, logger.Errorf("Unrecognized/Corrupted database schema.")
		}

		if len(res[0].Series) == 0 {
			// page out of range
			continue
		}

		serieRecords, err := serieToRecords(res[0].Series[0])
		if err != nil {
			return senml.SenML{}, 0, logger.Errorf("Error parsing points for source %v: %s", ds.Resource, err)
		}

		if perItems[i] != 0 { // influx ignores `limit 0`
			pack.Records = append(pack.Records, serieRecords...)
		}
	}

	// q.Limit overrides total
	if q.Limit > 0 && q.Limit < total {
		total = q.Limit
	}

	return pack, total, nil
}

// NtfCreated handles the creation of a new data source
func (s *InfluxStorage) NtfCreated(ds registry.DataSource, callback chan error) {
	s.prepare.Wait()

	callback <- nil
}

// NtfUpdated handles updates of a data source
func (s *InfluxStorage) NtfUpdated(oldDS registry.DataSource, newDS registry.DataSource, callback chan error) {
	s.prepare.Wait()

	if oldDS.Retention != newDS.Retention {
		err := s.ChangeRetentionPolicy(s.MeasurementName(oldDS.ID), s.FieldForType(oldDS.Type), oldDS.Retention, newDS.Retention)
		if err != nil {
			callback <- logger.Errorf("Error changing retention policy: %s", err)
			return
		}
		logger.Println("InfluxAggr: changed retenton policy for", newDS.ID)
	}

	callback <- nil
}

// NtfDeleted handles deletion of a data source
func (s *InfluxStorage) NtfDeleted(ds registry.DataSource, callback chan error) {
	s.prepare.Wait()

	_, err := s.QuerySprintf("DROP MEASUREMENT \"%s\"", s.MeasurementName(ds.ID))
	if err != nil {
		if strings.Contains(err.Error(), "measurement not found") {
			// Not an error, No data to delete.
			callback <- nil
			return
		}
		callback <- logger.Errorf("Error removing the historical data: %s", err)
		return
	}
	logger.Println("InfluxStorage: dropped measurements for", ds.ID)

	callback <- nil
}

// UTILITY FUNCTIONS

// QuerySprintf constructs a query for influxdb
func (s *InfluxStorage) QuerySprintf(format string, a ...interface{}) (res []influx.Result, err error) {
	logger.Debugln("Influx:", fmt.Sprintf(format, a...))
	q := influx.Query{
		Command:  fmt.Sprintf(format, a...),
		Database: s.config.database,
	}
	response, err := s.client.Query(q)
	if err != nil {
		return res, logger.Errorf("Request error: %v", err)
	}
	if response.Error() != nil {
		return res, logger.Errorf("Statement error: %v", response.Error())
	}

	return response.Results, nil
}

// CountSprintf constructs a counting query for influxdb
func (s *InfluxStorage) CountSprintf(format string, a ...interface{}) (int64, error) {
	res, err := s.QuerySprintf(format, a...)
	if err != nil {
		return 0, logger.Errorf("%s", err)
	}

	if len(res) < 1 {
		return 0, logger.Errorf("Unable to get count from database: response empty")
	}
	if len(res[0].Series) < 1 {
		// No data
		return 0, nil
	}
	if len(res[0].Series[0].Values) < 1 ||
		len(res[0].Series[0].Values[0]) < 2 {
		return 0, logger.Errorf("Unable to get count from database: bad response")
	}
	count, err := res[0].Series[0].Values[0][1].(json.Number).Int64()
	if err != nil {
		return 0, logger.Errorf("Unable to parse count from database response.")
	}
	return count, nil
}

//
func (s *InfluxStorage) ChangeRetentionPolicy(measurement, countField, oldRP, newRP string) error {
	count, err := s.CountSprintf("SELECT COUNT(%s) FROM %s GROUP BY *",
		countField, s.MeasurementNameFQ(oldRP, measurement))
	if err != nil {
		return logger.Errorf("Error counting historical data: %s", err)
	}
	if count == 0 {
		// no data to move
		return nil
	}

	retention, err := s.ParseDuration(newRP)
	if err != nil {
		return logger.Errorf("Error parsing retention period: %s: %s", newRP, err)
	}
	retention -= time.Minute // reduce 1m to avoid overshooting the new RP

	// formatting functions
	measurementNameTemp := func(uuid string) string {
		return fmt.Sprintf("temp_%s", uuid)
	}
	measurementNameTempFQ := func(uuid string) string {
		return fmt.Sprintf("%s.\"%s\".\"temp_%s\"", s.config.database, s.RetentionPolicyName(""), uuid)
	}

	tempUUID := uuid.NewV4().String()

	// Changing retention policy in four steps:
	// 1) keep required data in temp measurement
	_, err = s.QuerySprintf("SELECT * INTO %s FROM %s WHERE time > '%s'",
		measurementNameTempFQ(tempUUID), s.MeasurementNameFQ(oldRP, measurement), time.Now().UTC().Add(-retention).Format(time.RFC3339))
	if err != nil {
		return logger.Errorf("Error moving the historical data to new retention policy: %s", err)
	}
	// 2) delete the data from old measurement (on all RPs)
	_, err = s.QuerySprintf("DELETE FROM \"%s\"", measurement)
	if err != nil {
		return logger.Errorf("Error removing the historical data: %s", err)
	}
	// 3) move data from temp into new RP
	_, err = s.QuerySprintf("SELECT * INTO %s FROM %s",
		s.MeasurementNameFQ(newRP, measurement), measurementNameTempFQ(tempUUID))
	if err != nil {
		return logger.Errorf("Error moving the historical data to new retention policy: %s", err)
	}
	// 4) drop temp
	_, err = s.QuerySprintf("DROP MEASUREMENT \"%s\"", measurementNameTemp(tempUUID))
	if err != nil {
		if strings.Contains(err.Error(), "measurement not found") {
			// Not an error, No data to delete.
			return nil
		}
		return logger.Errorf("Error removing the historical data: %s", err)
	}
	return nil
}

func (s *InfluxStorage) ParseDuration(durationStr string) (time.Duration, error) {
	if durationStr == "" {
		return time.Since(time.Unix(0, 0)), nil
	}
	return influxql.ParseDuration(durationStr)
}

type influxStorageConfig struct {
	dsn         string
	database    string
	username    string
	password    string
	replication int
}

// initInfluxConf initializes the influxdb configuration
func initInfluxConf(DSN string) (*influxStorageConfig, error) {
	// Parse config's DSN string
	PDSN, err := url.Parse(DSN)
	if err != nil {
		return nil, logger.Errorf("%s", err)
	}
	// Validate
	if PDSN.Host == "" {
		return nil, logger.Errorf("Influxdb config: host:port in the URL must be not empty")
	}
	if PDSN.Path == "" {
		return nil, logger.Errorf("Influxdb config: db must be not empty")
	}

	var c influxStorageConfig
	c.dsn = fmt.Sprintf("%v://%v", PDSN.Scheme, PDSN.Host)
	c.database = strings.Trim(PDSN.Path, "/")
	// Optional username and password
	if PDSN.User != nil {
		c.username = PDSN.User.Username()
		c.password, _ = PDSN.User.Password()
	}

	return &c, nil
}

// prepareStorage prepares the backend for storage
func (s *InfluxStorage) prepareStorage() {
	// wait for influxdb
	for interval := 5; ; interval *= 2 {
		if interval >= 60 {
			interval = 60
		}
		_, version, err := s.client.Ping(influxPingTimeout)
		if err != nil {
			logger.Printf("InfluxStorage: Unable to reach influxdb backend: %s", err)
			time.Sleep(time.Duration(interval) * time.Second)
			continue
		}
		logger.Printf("InfluxStorage: Connected to InfluxDB %s", version)
		break
	}

	// create retention policies
	for _, period := range s.retentionPeriods {
		_, err := s.QuerySprintf("CREATE RETENTION POLICY \"%s\" ON \"%s\" DURATION %v REPLICATION %d",
			s.RetentionPolicyName(period), s.config.database, period, s.config.replication)
		if err != nil {
			// TODO check database before this?
			logger.Fatalf("Error creating retention policies: %s", err)
		}
		logger.Printf("InfluxStorage: Created retention policy for period: %s", period)
	}

	s.prepare.Done()
}

// MeasurementName returns formatted measurement name for a given data source
func (s *InfluxStorage) MeasurementName(id string) string {
	return fmt.Sprintf("data_%s", id)
}

// MeasurementNameFQ returns formatted fully-qualified measurement name
func (s *InfluxStorage) MeasurementNameFQ(retention, measurementName string) string {
	return fmt.Sprintf("\"%s\".\"%s\".\"%s\"", s.config.database, s.RetentionPolicyName(retention), measurementName)
}

// RetentionPolicyName returns formatted retention policy name for a given period
func (s *InfluxStorage) RetentionPolicyName(period string) string {
	if period == "" {
		return "autogen" // default retention policy name
	}
	return fmt.Sprintf("policy_%s", period)
}

// FieldForType returns the field-name for HDS data types
func (s *InfluxStorage) FieldForType(t string) string {
	switch t {
	case common.FLOAT:
		return "value"
	case common.STRING:
		return "stringValue"
	case common.BOOL:
		return "booleanValue"
	}
	return ""
}

// Database returns database name
func (s *InfluxStorage) Database() string {
	return s.config.database
}

// Replication returns Influxdb Replication factor
func (s *InfluxStorage) Replication() int {
	return s.config.replication
}

// pointsFromRow converts Influxdb rows to HDS data points
func serieToRecords(r models.Row) ([]senml.SenMLRecord, error) {
	var records []senml.SenMLRecord

	for _, e := range r.Values {
		var record senml.SenMLRecord

		// fields and tags
		for i, v := range e {
			// point with nil column
			if v == nil {
				continue
			}
			switch r.Columns[i] {
			case "time":
				if val, ok := v.(string); ok {
					t, err := time.Parse(time.RFC3339, val)
					if err != nil {
						return nil, logger.Errorf("Invalid time format: %v", val)
					}
					record.Time = float64(t.UnixNano()) / 1000000000
				} else {
					return nil, logger.Errorf("Interface conversion error. time not string?")
				}
			case "name":
				if val, ok := v.(string); ok {
					record.Name = val
				} else {
					return nil, logger.Errorf("Interface conversion error. name not string?")
				}
			case "value":
				if val, err := v.(json.Number).Float64(); err == nil {
					record.Value = &val
				} else {
					return nil, logger.Errorf("%s", err)
				}
			case "booleanValue":
				if val, ok := v.(bool); ok {
					record.BoolValue = &val
				} else {
					return nil, logger.Errorf("Interface conversion error. booleanValue not bool?")
				}
			case "stringValue":
				if val, ok := v.(string); ok {
					record.StringValue = val
				} else {
					return nil, logger.Errorf("Interface conversion error. stringValue not string?")
				}
			case "units":
				if val, ok := v.(string); ok {
					record.Unit = val
				} else {
					return nil, logger.Errorf("Interface conversion error. units not string?")
				}
			} // endswitch
		}
		records = append(records, record)
	}

	return records, nil
}
