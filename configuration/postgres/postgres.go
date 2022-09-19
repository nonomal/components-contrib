/*
Copyright 2021 The Dapr Authors
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgconn"
	"github.com/jackc/pgx/v4/pgxpool"
	_ "github.com/jackc/pgx/v4/stdlib"

	"github.com/dapr/components-contrib/configuration"
	"github.com/dapr/kit/logger"
)

type ConfigurationStore struct {
	metadata             metadata
	client               *pgxpool.Pool
	logger               logger.Logger
	configLock           sync.Mutex
	subscribeStopChanMap sync.Map
	ActiveSubscriptions  map[string]*subscription
}

type subscription struct {
	uuid    string
	trigger string
	keys    []string
}

const (
	configtablekey               = "table"
	connMaxIdleTimeKey           = "connMaxIdleTime"
	connectionStringKey          = "connectionString"
	ErrorMissingTableName        = "missing postgreSQL configuration table name"
	InfoStartInit                = "initializing postgreSQL configuration store"
	ErrorMissingConnectionString = "missing postgreSQL connection string"
	ErrorAlreadyInitialized      = "PostgreSQL configuration store already initialized"
	ErrorMissinMaxTimeout        = "missing PostgreSQL maxTimeout setting in configuration"
	QueryTableExists             = "SELECT EXISTS (SELECT FROM pg_tables where tablename = $1)"
	maxIdentifierLength          = 64 // https://www.postgresql.org/docs/current/limits.html
	ErrorTooLongFieldLength      = "field name is too long"
)

var allowedChars = regexp.MustCompile(`^[a-zA-Z0-9./_]*$`)

func NewPostgresConfigurationStore(logger logger.Logger) configuration.Store {
	logger.Debug("Instantiating PostgreSQL configuration store")
	return &ConfigurationStore{
		logger:               logger,
		subscribeStopChanMap: sync.Map{},
		configLock:           sync.Mutex{},
	}
}

func (p *ConfigurationStore) Init(metadata configuration.Metadata) error {
	p.logger.Debug(InfoStartInit)

	if p.client != nil {
		return fmt.Errorf(ErrorAlreadyInitialized)
	}
	if m, err := parseMetadata(metadata); err != nil {
		p.logger.Error(err)
		return err
	} else {
		p.metadata = m
	}
	p.ActiveSubscriptions = make(map[string]*subscription)
	ctx, cancel := context.WithTimeout(context.Background(), p.metadata.maxIdleTime)
	defer cancel()
	client, err := Connect(ctx, p.metadata.connectionString, p.metadata.maxIdleTime)
	if err != nil {
		return err
	}
	p.client = client
	pingErr := p.client.Ping(ctx)
	if pingErr != nil {
		return pingErr
	}
	// check if table exists
	exists := false
	err = p.client.QueryRow(ctx, QueryTableExists, p.metadata.configTable).Scan(&exists)
	if err != nil {
		return err
	}
	return nil
}

func (p *ConfigurationStore) Get(ctx context.Context, req *configuration.GetRequest) (*configuration.GetResponse, error) {
	if err := validateInput(req.Keys); err != nil {
		p.logger.Error(err)
		return nil, err
	}
	query, params, err := buildQuery(req, p.metadata.configTable)
	if err != nil {
		p.logger.Error(err)
		return nil, err
	}
	rows, err := p.client.Query(ctx, query, params...)
	if err != nil {
		// If no rows exist, return an empty response, otherwise return the error.
		if err == sql.ErrNoRows {
			return &configuration.GetResponse{}, nil
		}
		return nil, err
	}
	items := make(map[string]*configuration.Item)
	for i := 0; rows.Next(); i++ {
		var item configuration.Item
		var key string
		var metadata []byte
		v := make(map[string]string)

		if err := rows.Scan(&key, &item.Value, &item.Version, &metadata); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(metadata, &v); err != nil {
			return nil, err
		}
		item.Metadata = v
		if item.Value != "" {
			items[key] = &item
		}
	}
	return &configuration.GetResponse{
		Items: items,
	}, nil
}

func (p *ConfigurationStore) Subscribe(ctx context.Context, req *configuration.SubscribeRequest, handler configuration.UpdateHandler) (string, error) {
	var triggers []string
	for k, v := range req.Metadata {
		if res := strings.EqualFold("trigger", k); res {
			triggers = append(triggers, v)
		}
	}

	if len(triggers) == 0 {
		return "", fmt.Errorf("cannot subscribe to empty trigger")
	}

	var subscribeID string
	p.configLock.Lock()
	for _, trigger := range triggers {
		notificationChannel := "listen " + trigger
		if sub, isActive := p.isSubscriptionActive(req); isActive {
			if oldStopChan, ok := p.subscribeStopChanMap.Load(sub); ok {
				close(oldStopChan.(chan struct{}))
			}
		}
		stop := make(chan struct{})
		subscribeID = uuid.New().String()
		p.subscribeStopChanMap.Store(subscribeID, stop)
		p.ActiveSubscriptions[trigger] = &subscription{
			uuid:    subscribeID,
			trigger: trigger,
			keys:    req.Keys,
		}
		go p.doSubscribe(ctx, req, handler, notificationChannel, trigger, subscribeID, stop)
	}
	p.configLock.Unlock()
	return subscribeID, nil
}

func (p *ConfigurationStore) Unsubscribe(ctx context.Context, req *configuration.UnsubscribeRequest) error {
	p.configLock.Lock()
	defer p.configLock.Unlock()
	if oldStopChan, ok := p.subscribeStopChanMap.Load(req.ID); ok {
		p.subscribeStopChanMap.Delete(req.ID)
		close(oldStopChan.(chan struct{}))
		for k, v := range p.ActiveSubscriptions {
			if v.uuid == req.ID {
				channel := "unlisten " + v.trigger
				conn, err := p.client.Acquire(ctx)
				if err != nil {
					return fmt.Errorf("Error acquiring connection: %v", err)
				}
				defer conn.Release()
				_, err = conn.Exec(ctx, channel)
				if err != nil {
					return fmt.Errorf("Error unlistening to channel: %v", err)
				}
				delete(p.ActiveSubscriptions, k)
			}
		}
		return nil
	}
	return fmt.Errorf("unable to find subscription with ID : %v", req.ID)
}

func (p *ConfigurationStore) doSubscribe(ctx context.Context, req *configuration.SubscribeRequest, handler configuration.UpdateHandler, channel string, trigger string, subscription string, stop chan struct{}) {
	conn, err := p.client.Acquire(ctx)
	if err != nil {
		p.logger.Errorf("Error acquiring connection:", err)
		return
	}
	defer conn.Release()
	_, err = conn.Exec(ctx, channel)
	if err != nil {
		p.logger.Errorf("Error listening to channel:", err)
		return
	}

	for {
		notification, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			if !(pgconn.Timeout(err) || errors.Is(ctx.Err(), context.Canceled)) {
				p.logger.Errorf("Error waiting for notification:", err)
			}
			return
		}
		p.handleSubscribedChange(ctx, handler, notification, trigger, subscription)
	}
}

func (p *ConfigurationStore) handleSubscribedChange(ctx context.Context, handler configuration.UpdateHandler, msg *pgconn.Notification, trigger string, subscription string) {
	defer func() {
		if err := recover(); err != nil {
			p.logger.Errorf("panic in handleSubscribedChange method and recovered: %s", err)
		}
	}()
	payload := make(map[string]interface{})
	err := json.Unmarshal([]byte(msg.Payload), &payload)
	if err != nil {
		p.logger.Errorf("Error in UnMarshal: ", err)
		return
	}

	var key, value, version string
	m := make(map[string]string)
	// trigger should encapsulate the row in "data" field in the notification
	row := reflect.ValueOf(payload["data"])
	if row.Kind() == reflect.Map {
		for _, k := range row.MapKeys() {
			v := row.MapIndex(k)
			strKey := k.Interface().(string)
			switch strings.ToLower(strKey) {
			case "key":
				key = v.Interface().(string)
				if yes := p.isSubscribed(trigger, key, subscription); !yes {
					p.logger.Debugf("Ignoring notification for %v", key)
					return
				}
			case "value":
				value = v.Interface().(string)
			case "version":
				version = v.Interface().(string)
			case "metadata":
				a := v.Interface().(map[string]interface{})
				for k, v := range a {
					m[k] = v.(string)
				}
			}
		}
	}
	e := &configuration.UpdateEvent{
		Items: map[string]*configuration.Item{
			key: {
				Value:    value,
				Version:  version,
				Metadata: m,
			},
		},
		ID: subscription,
	}
	err = handler(ctx, e)
	if err != nil {
		p.logger.Errorf("fail to call handler to notify event for configuration update subscribe: %s", err)
	}
}

func parseMetadata(cmetadata configuration.Metadata) (metadata, error) {
	m := metadata{}

	if val, ok := cmetadata.Properties[connectionStringKey]; ok && val != "" {
		m.connectionString = val
	} else {
		return m, fmt.Errorf(ErrorMissingConnectionString)
	}

	if tbl, ok := cmetadata.Properties[configtablekey]; ok && tbl != "" {
		if !allowedChars.MatchString(tbl) {
			return m, fmt.Errorf("invalid table name : '%v'. non-alphanumerics are not supported", tbl)
		}
		if len(tbl) > maxIdentifierLength {
			return m, fmt.Errorf(ErrorTooLongFieldLength+" - tableName : '%v'. max allowed field length is %v ", tbl, maxIdentifierLength)
		}
		m.configTable = tbl
	} else {
		return m, fmt.Errorf(ErrorMissingTableName)
	}

	// configure maxTimeout if provided
	if mxTimeout, ok := cmetadata.Properties[connMaxIdleTimeKey]; ok && mxTimeout != "" {
		if t, err := time.ParseDuration(mxTimeout); err == nil {
			m.maxIdleTime = t
		} else {
			return m, fmt.Errorf(ErrorMissinMaxTimeout)
		}
	}
	return m, nil
}

func Connect(ctx context.Context, conn string, maxTimeout time.Duration) (*pgxpool.Pool, error) {
	pool, err := pgxpool.Connect(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("postgres configuration store connection error : %s", err)
	}
	pingErr := pool.Ping(ctx)
	if pingErr != nil {
		return nil, fmt.Errorf("postgres configuration store ping error : %s", pingErr)
	}
	return pool, nil
}

func buildQuery(req *configuration.GetRequest, configTable string) (string, []interface{}, error) {
	var query string
	var params []interface{}
	if len(req.Keys) == 0 {
		query = "SELECT * FROM " + configTable
	} else {
		var queryBuilder strings.Builder
		queryBuilder.WriteString("SELECT * FROM " + configTable + " WHERE KEY IN (")
		var paramWildcard []string
		paramPosition := 1
		for _, v := range req.Keys {
			paramWildcard = append(paramWildcard, "$"+strconv.Itoa(paramPosition))
			params = append(params, v)
			paramPosition++
		}
		queryBuilder.WriteString(strings.Join(paramWildcard, " , "))
		queryBuilder.WriteString(")")

		query = queryBuilder.String()

		if len(req.Metadata) > 0 {
			var s strings.Builder
			i, j := len(req.Metadata), 0
			s.WriteString(" AND ")
			for k, v := range req.Metadata {
				temp := "$" + strconv.Itoa(paramPosition) + " = " + "$" + strconv.Itoa(paramPosition+1)
				s.WriteString(temp)
				params = append(params, k, v)
				paramPosition += 2
				if j++; j < i {
					s.WriteString(" AND ")
				}
			}
			query += s.String()
		}
	}
	return query, params, nil
}

func (p *ConfigurationStore) isSubscriptionActive(req *configuration.SubscribeRequest) (string, bool) {
	for _, trigger := range req.Metadata {
		for key2, sub := range p.ActiveSubscriptions {
			if res := strings.EqualFold(trigger, key2); res {
				return sub.uuid, true
			}
		}
	}
	return " ", false
}

func (p *ConfigurationStore) isSubscribed(trigger string, key string, subscription string) bool {
	for t, s := range p.ActiveSubscriptions {
		if t == trigger && s.uuid == subscription {
			for _, k := range s.keys {
				if k == key {
					return true
				}
			}
		}
	}
	return false
}

func validateInput(keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	for _, key := range keys {
		if !allowedChars.MatchString(key) {
			return fmt.Errorf("invalid key : '%v'", key)
		}
	}
	return nil
}
