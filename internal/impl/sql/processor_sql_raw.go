// Copyright 2024 Redpanda Data, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sql

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

	"github.com/Jeffail/shutdown"

	"github.com/redpanda-data/benthos/v4/public/bloblang"
	"github.com/redpanda-data/benthos/v4/public/service"
)

// RawProcessorConfig returns a config spec for an sql_raw processor.
func RawProcessorConfig() *service.ConfigSpec {
	spec := service.NewConfigSpec().
		Stable().
		Categories("Integration").
		Summary("Runs an arbitrary SQL query against a database and (optionally) returns the result as an array of objects, one for each row returned.").
		Description(`
If the query fails to execute then the message will remain unchanged and the error can be caught using xref:configuration:error_handling.adoc[error handling methods].`).
		Field(driverField).
		Field(dsnField).
		Field(rawQueryField().
			Example("INSERT INTO footable (foo, bar, baz) VALUES (?, ?, ?);").
			Example("SELECT * FROM footable WHERE user_id = $1;")).
		Field(service.NewBoolField("unsafe_dynamic_query").
			Description("Whether to enable xref:configuration:interpolation.adoc#bloblang-queries[interpolation functions] in the query. Great care should be made to ensure your queries are defended against injection attacks.").
			Advanced().
			Default(false)).
		Field(service.NewBloblangField("args_mapping").
			Description("An optional xref:guides:bloblang/about.adoc[Bloblang mapping] which should evaluate to an array of values matching in size to the number of placeholder arguments in the field `query`.").
			Example("root = [ this.cat.meow, this.doc.woofs[0] ]").
			Example(`root = [ meta("user.id") ]`).
			Optional()).
		Field(service.NewBoolField("exec_only").
			Description("Whether the query result should be discarded. When set to `true` the message contents will remain unchanged, which is useful in cases where you are executing inserts, updates, etc.").
			Default(false))

	for _, f := range connFields() {
		spec = spec.Field(f)
	}

	spec = spec.Version("3.65.0").
		Example(
			"Table Insert (MySQL)",
			"The following example inserts rows into the table footable with the columns foo, bar and baz populated with values extracted from messages.",
			`
pipeline:
  processors:
    - sql_raw:
        driver: mysql
        dsn: foouser:foopassword@tcp(localhost:3306)/foodb
        query: "INSERT INTO footable (foo, bar, baz) VALUES (?, ?, ?);"
        args_mapping: '[ document.foo, document.bar, meta("kafka_topic") ]'
        exec_only: true
`,
		).
		Example(
			"Table Query (PostgreSQL)",
			`Here we query a database for columns of footable that share a `+"`user_id`"+` with the message field `+"`user.id`"+`. A `+"xref:components:processors/branch.adoc[`branch` processor]"+` is used in order to insert the resulting array into the original message at the path `+"`foo_rows`"+`.`,
			`
pipeline:
  processors:
    - branch:
        processors:
          - sql_raw:
              driver: postgres
              dsn: postgres://foouser:foopass@localhost:5432/testdb?sslmode=disable
              query: "SELECT * FROM footable WHERE user_id = $1;"
              args_mapping: '[ this.user.id ]'
        result_map: 'root.foo_rows = this'
`,
		)
	return spec
}

func init() {
	err := service.RegisterBatchProcessor(
		"sql_raw", RawProcessorConfig(),
		func(conf *service.ParsedConfig, mgr *service.Resources) (service.BatchProcessor, error) {
			return NewSQLRawProcessorFromConfig(conf, mgr)
		})
	if err != nil {
		panic(err)
	}
}

//------------------------------------------------------------------------------

type sqlRawProcessor struct {
	db    *sql.DB
	dbMut sync.RWMutex

	queryStatic string
	queryDyn    *service.InterpolatedString
	onlyExec    bool

	argsMapping   *bloblang.Executor
	argsConverter argsConverter

	logger  *service.Logger
	shutSig *shutdown.Signaller
}

// NewSQLRawProcessorFromConfig returns an internal sql_raw processor.
func NewSQLRawProcessorFromConfig(conf *service.ParsedConfig, mgr *service.Resources) (*sqlRawProcessor, error) {
	driverStr, err := conf.FieldString("driver")
	if err != nil {
		return nil, err
	}

	dsnStr, err := conf.FieldString("dsn")
	if err != nil {
		return nil, err
	}

	queryStatic, err := conf.FieldString("query")
	if err != nil {
		return nil, err
	}

	var queryDyn *service.InterpolatedString
	if unsafeDyn, err := conf.FieldBool("unsafe_dynamic_query"); err != nil {
		return nil, err
	} else if unsafeDyn {
		if queryDyn, err = conf.FieldInterpolatedString("query"); err != nil {
			return nil, err
		}
	}

	onlyExec, err := conf.FieldBool("exec_only")
	if err != nil {
		return nil, err
	}

	var argsMapping *bloblang.Executor
	if conf.Contains("args_mapping") {
		if argsMapping, err = conf.FieldBloblang("args_mapping"); err != nil {
			return nil, err
		}
	}

	connSettings, err := connSettingsFromParsed(conf, mgr)
	if err != nil {
		return nil, err
	}

	var argsConverter argsConverter
	if driverStr == "postgres" {
		argsConverter = bloblValuesToPgSQLValues
	} else {
		argsConverter = func(v []any) []any { return v }
	}

	return newSQLRawProcessor(mgr.Logger(), driverStr, dsnStr, queryStatic, queryDyn, onlyExec, argsMapping, argsConverter, connSettings)
}

func newSQLRawProcessor(
	logger *service.Logger,
	driverStr, dsnStr string,
	queryStatic string,
	queryDyn *service.InterpolatedString,
	onlyExec bool,
	argsMapping *bloblang.Executor,
	argsConverter argsConverter,
	connSettings *connSettings,
) (*sqlRawProcessor, error) {
	s := &sqlRawProcessor{
		logger:        logger,
		shutSig:       shutdown.NewSignaller(),
		queryStatic:   queryStatic,
		queryDyn:      queryDyn,
		onlyExec:      onlyExec,
		argsMapping:   argsMapping,
		argsConverter: argsConverter,
	}

	var err error
	if s.db, err = sqlOpenWithReworks(logger, driverStr, dsnStr); err != nil {
		return nil, err
	}
	connSettings.apply(context.Background(), s.db, s.logger)

	go func() {
		<-s.shutSig.HardStopChan()

		s.dbMut.Lock()
		_ = s.db.Close()
		s.dbMut.Unlock()

		s.shutSig.TriggerHasStopped()
	}()
	return s, nil
}

func (s *sqlRawProcessor) ProcessBatch(ctx context.Context, batch service.MessageBatch) ([]service.MessageBatch, error) {
	s.dbMut.RLock()
	defer s.dbMut.RUnlock()

	var argsExec *service.MessageBatchBloblangExecutor
	if s.argsMapping != nil {
		argsExec = batch.BloblangExecutor(s.argsMapping)
	}

	batch = batch.Copy()
	for i, msg := range batch {
		var args []any
		if argsExec != nil {
			resMsg, err := argsExec.Query(i)
			if err != nil {
				s.logger.Debugf("Arguments mapping failed: %v", err)
				msg.SetError(err)
				continue
			}

			iargs, err := resMsg.AsStructured()
			if err != nil {
				s.logger.Debugf("Mapping returned non-structured result: %v", err)
				msg.SetError(fmt.Errorf("mapping returned non-structured result: %w", err))
				continue
			}

			var ok bool
			if args, ok = iargs.([]any); !ok {
				s.logger.Debugf("Mapping returned non-array result: %T", iargs)
				msg.SetError(fmt.Errorf("mapping returned non-array result: %T", iargs))
				continue
			}
			args = s.argsConverter(args)
		}

		queryStr := s.queryStatic
		if s.queryDyn != nil {
			var err error
			if queryStr, err = batch.TryInterpolatedString(i, s.queryDyn); err != nil {
				s.logger.Errorf("Query interoplation error: %v", err)
				msg.SetError(fmt.Errorf("query interpolation error: %w", err))
				continue
			}
		}

		if s.onlyExec {
			if _, err := s.db.ExecContext(ctx, queryStr, args...); err != nil {
				s.logger.Debugf("Failed to run query: %v", err)
				msg.SetError(err)
				continue
			}
		} else {
			rows, err := s.db.QueryContext(ctx, queryStr, args...)
			if err != nil {
				s.logger.Debugf("Failed to run query: %v", err)
				msg.SetError(err)
				continue
			}

			if jArray, err := sqlRowsToArray(rows); err != nil {
				s.logger.Debugf("Failed to convert rows: %v", err)
				msg.SetError(err)
			} else {
				msg.SetStructuredMut(jArray)
			}
		}
	}
	return []service.MessageBatch{batch}, nil
}

func (s *sqlRawProcessor) Close(ctx context.Context) error {
	s.shutSig.TriggerHardStop()
	select {
	case <-s.shutSig.HasStoppedChan():
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}
