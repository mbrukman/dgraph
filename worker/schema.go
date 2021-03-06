/*
 * Copyright 2017-2018 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package worker

import (
	otrace "go.opencensus.io/trace"
	"golang.org/x/net/context"

	"github.com/dgraph-io/dgo/protos/api"
	"github.com/dgraph-io/dgraph/conn"
	"github.com/dgraph-io/dgraph/protos/pb"
	"github.com/dgraph-io/dgraph/schema"
	"github.com/dgraph-io/dgraph/types"
	"github.com/dgraph-io/dgraph/x"
)

var (
	emptySchemaResult pb.SchemaResult
)

type resultErr struct {
	result *pb.SchemaResult
	err    error
}

// getSchema iterates over all predicates and populates the asked fields, if list of
// predicates is not specified, then all the predicates belonging to the group
// are returned
func getSchema(ctx context.Context, s *pb.SchemaRequest) (*pb.SchemaResult, error) {
	ctx, span := otrace.StartSpan(ctx, "worker.getSchema")
	defer span.End()

	var result pb.SchemaResult
	var predicates []string
	var fields []string
	if len(s.Predicates) > 0 {
		predicates = s.Predicates
	} else {
		predicates = schema.State().Predicates()
	}
	if len(s.Fields) > 0 {
		fields = s.Fields
	} else {
		fields = []string{"type", "index", "tokenizer", "reverse", "count", "list", "upsert",
			"lang"}
	}

	for _, attr := range predicates {
		// This can happen after a predicate is moved. We don't delete predicate from schema state
		// immediately. So lets ignore this predicate.
		if !groups().ServesTablet(attr) {
			continue
		}
		if schemaNode := populateSchema(attr, fields); schemaNode != nil {
			result.Schema = append(result.Schema, schemaNode)
		}
	}
	return &result, nil
}

// populateSchema returns the information of asked fields for given attribute
func populateSchema(attr string, fields []string) *api.SchemaNode {
	var schemaNode api.SchemaNode
	var typ types.TypeID
	var err error
	if typ, err = schema.State().TypeOf(attr); err != nil {
		// schema is not defined
		return nil
	}
	schemaNode.Predicate = attr
	for _, field := range fields {
		switch field {
		case "type":
			schemaNode.Type = typ.Name()
		case "index":
			schemaNode.Index = schema.State().IsIndexed(attr)
		case "tokenizer":
			if schema.State().IsIndexed(attr) {
				schemaNode.Tokenizer = schema.State().TokenizerNames(attr)
			}
		case "reverse":
			schemaNode.Reverse = schema.State().IsReversed(attr)
		case "count":
			schemaNode.Count = schema.State().HasCount(attr)
		case "list":
			schemaNode.List = schema.State().IsList(attr)
		case "upsert":
			schemaNode.Upsert = schema.State().HasUpsert(attr)
		case "lang":
			schemaNode.Lang = schema.State().HasLang(attr)
		default:
			//pass
		}
	}
	return &schemaNode
}

// addToSchemaMap groups the predicates by group id, if list of predicates is
// empty then it adds all known groups
func addToSchemaMap(schemaMap map[uint32]*pb.SchemaRequest, schema *pb.SchemaRequest) {
	for _, attr := range schema.Predicates {
		gid := groups().BelongsTo(attr)
		s := schemaMap[gid]
		if s == nil {
			s = &pb.SchemaRequest{GroupId: gid}
			s.Fields = schema.Fields
			schemaMap[gid] = s
		}
		s.Predicates = append(s.Predicates, attr)
	}
	if len(schema.Predicates) > 0 {
		return
	}
	// TODO: Janardhan - node shouldn't serve any request until membership
	// information is synced, should we fail health check till then ?
	gids := groups().KnownGroups()
	for _, gid := range gids {
		if gid == 0 {
			continue
		}
		s := schemaMap[gid]
		if s == nil {
			s = &pb.SchemaRequest{GroupId: gid}
			s.Fields = schema.Fields
			schemaMap[gid] = s
		}
	}
}

// If the current node serves the group serve the schema or forward
// to relevant node
// TODO: Janardhan - if read fails try other servers serving same group
func getSchemaOverNetwork(ctx context.Context, gid uint32, s *pb.SchemaRequest, ch chan resultErr) {
	if groups().ServesGroup(gid) {
		schema, e := getSchema(ctx, s)
		ch <- resultErr{result: schema, err: e}
		return
	}

	pl := groups().Leader(gid)
	if pl == nil {
		ch <- resultErr{err: conn.ErrNoConnection}
		return
	}
	conn := pl.Get()
	c := pb.NewWorkerClient(conn)
	schema, e := c.Schema(ctx, s)
	ch <- resultErr{result: schema, err: e}
}

// GetSchemaOverNetwork checks which group should be serving the schema
// according to fingerprint of the predicate and sends it to that instance.
func GetSchemaOverNetwork(ctx context.Context, schema *pb.SchemaRequest) ([]*api.SchemaNode, error) {
	ctx, span := otrace.StartSpan(ctx, "worker.GetSchemaOverNetwork")
	defer span.End()

	if err := x.HealthCheck(); err != nil {
		return nil, err
	}

	// Map of groupd id => Predicates for that group.
	schemaMap := make(map[uint32]*pb.SchemaRequest)
	addToSchemaMap(schemaMap, schema)

	results := make(chan resultErr, len(schemaMap))
	var schemaNodes []*api.SchemaNode

	for gid, s := range schemaMap {
		if gid == 0 {
			return schemaNodes, errUnservedTablet
		}
		go getSchemaOverNetwork(ctx, gid, s, results)
	}

	// wait for all the goroutines to reply back.
	// we return if an error was returned or the parent called ctx.Done()
	for i := 0; i < len(schemaMap); i++ {
		select {
		case r := <-results:
			if r.err != nil {
				return nil, r.err
			}
			schemaNodes = append(schemaNodes, r.result.Schema...)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	return schemaNodes, nil
}

// Schema is used to get schema information over the network on other instances.
func (w *grpcWorker) Schema(ctx context.Context, s *pb.SchemaRequest) (*pb.SchemaResult, error) {
	if ctx.Err() != nil {
		return &emptySchemaResult, ctx.Err()
	}

	if !groups().ServesGroup(s.GroupId) {
		return &emptySchemaResult, x.Errorf("This server doesn't serve group id: %v", s.GroupId)
	}
	return getSchema(ctx, s)
}
