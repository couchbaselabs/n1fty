// Copyright (c) 2016 Couchbase, Inc.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//     http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an "AS IS"
// BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express
// or implied. See the License for the specific language governing
// permissions and limitations under the License.

// n1fty is the integration of N1ql with FTs Yndexing.

package n1fty

// TODO: GSI has a feature to spill out resultsets to disk, perhaps
// when the n1ql entryChannel consumer is too slow.  Need to see if we
// need something similar one day.

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/blevesearch/bleve"

	"github.com/couchbase/cbft"
	"github.com/couchbase/cbgt"
	"github.com/couchbase/gocb"
	gocbCBFT "github.com/couchbase/gocb/cbft"

	"github.com/couchbase/query/datastore"
	"github.com/couchbase/query/errors"
	"github.com/couchbase/query/expression"
	"github.com/couchbase/query/expression/parser"
	"github.com/couchbase/query/logging"
	"github.com/couchbase/query/timestamp"
	"github.com/couchbase/query/value"
)

// How long in msecs to cache a previous metadata refresh.
var REFRESH_DURATION_MSEC = 5

var INDEXER_PROVIDER_NAME = "fts"

var HttpGet func(url string) (resp *http.Response, err error) = http.Get

// An indexer implements the n1ql datastore.Indexer interface, and
// focuses on just FTS indexes.
type indexer struct {
	clusterURL string
	namespace  string // aka pool
	keyspace   string // aka bucket

	// The startTime is also used to target each indexer at a
	// different ftsEndpoint.
	startTime time.Time

	logPrefix string

	cluster *gocb.Cluster

	rw sync.RWMutex // Protects the fields that follow.

	lastRefreshStartTime time.Time

	// The following fields are immutable, and must be cloned on
	// "mutation".  The following should either be all nil (when no
	// buckets or no FTS services), or all non-nil together.

	indexIds   []string // All index id's.
	indexNames []string // All index names.

	allIndexes []datastore.Index        // []*index
	priIndexes []datastore.PrimaryIndex // []*index

	mapIndexesById   map[string]*index
	mapIndexesByName map[string]*index
}

// An index implements the n1ql datastore.Index interface,
// representing a part of an FTS index.  One FTS index can result in 1
// or more datastore.Index instances.  This is because an FTS index
// defines a mapping that can contain more than one field.
//
// The fields of an index are immutable.
type index struct {
	indexer *indexer
	id      string
	name    string

	indexDef *cbgt.IndexDef

	typeField string

	fieldPath   string // Ex: "locations.address.city".
	indexFields []*indexField

	seekKeyExprs  expression.Expressions
	rangeKeyExprs expression.Expressions
	conditionExpr expression.Expression

	isPrimary bool
}

// An indexField represents a bleve index field.
type indexField struct {
	docType   string
	isDefault bool // True when docType is for the default mapping.
	field     *bleve.FieldMapping
}

// ----------------------------------------------------

func NewFTSIndexer(clusterURL, namespace, keyspace string) (
	datastore.Indexer, errors.Error) {
	logging.Infof("NewFTSIndexer clusterURL: %s, namespace: %s, keyspace: %s",
		clusterURL, namespace, keyspace)

	cluster, err := gocb.Connect(clusterURL)
	if err != nil {
		logging.Infof("NewFTSIndexer clusterURL: %s, namespace: %s, keyspace: %s, err: %v",
			clusterURL, namespace, keyspace, err)
		return nil, errors.NewError(err, "NewFTSIndexer gocb.Connect")
	}

	cluster.Authenticate(gocb.ClusterAuthenticator{ // TODO.
		Buckets:  gocb.BucketAuthenticatorMap{},
		Username: "Administrator",
		Password: "password",
	})

	// TODO: Need to configure cluster authenticator?

	ks := &indexer{
		clusterURL: clusterURL,
		namespace:  namespace,
		keyspace:   keyspace,
		startTime:  time.Now(),
		cluster:    cluster,
	}

	return ks, nil
}

// ----------------------------------------------------

func (indexer *indexer) KeyspaceId() string {
	return indexer.keyspace
}

func (indexer *indexer) Name() datastore.IndexType {
	return datastore.IndexType(INDEXER_PROVIDER_NAME)
}

// Ids of the current set of FTS indexes.
func (indexer *indexer) IndexIds() ([]string, errors.Error) {
	if err := indexer.maybeRefresh(false); err != nil {
		return nil, err
	}

	indexer.rw.RLock()
	indexIds := indexer.indexIds
	indexer.rw.RUnlock()

	return indexIds, nil
}

// Names of the current set of FTS indexes.
func (indexer *indexer) IndexNames() ([]string, errors.Error) {
	if err := indexer.maybeRefresh(false); err != nil {
		return nil, err
	}

	indexer.rw.RLock()
	indexNames := indexer.indexNames
	indexer.rw.RUnlock()

	return indexNames, nil
}

// Find an index by id.
func (indexer *indexer) IndexById(id string) (datastore.Index, errors.Error) {
	var index datastore.Index
	var ok bool

	indexer.rw.RLock()
	if indexer.mapIndexesById != nil {
		index, ok = indexer.mapIndexesById[id]
	}
	indexer.rw.RUnlock()

	if !ok {
		return nil, errors.NewError(nil,
			fmt.Sprintf("FTS index with id %v not found", id))
	}

	return index, nil
}

// Find an index by name.
func (indexer *indexer) IndexByName(name string) (datastore.Index, errors.Error) {
	var index datastore.Index
	var ok bool

	indexer.rw.RLock()
	if indexer.mapIndexesByName != nil {
		index, ok = indexer.mapIndexesByName[name]
	}
	indexer.rw.RUnlock()

	if !ok {
		return nil, errors.NewError(nil,
			fmt.Sprintf("FTS index named %v not found", name))
	}

	return index, nil
}

// Return the latest set of all FTS indexes.
func (indexer *indexer) Indexes() ([]datastore.Index, errors.Error) {
	if err := indexer.maybeRefresh(false); err != nil {
		return nil, err
	}

	indexer.rw.RLock()
	allIndexes := indexer.allIndexes
	indexer.rw.RUnlock()

	return allIndexes, nil
}

// Returns the latest set of FTS primary indexes.
func (indexer *indexer) PrimaryIndexes() ([]datastore.PrimaryIndex, errors.Error) {
	if err := indexer.maybeRefresh(false); err != nil {
		return nil, err
	}

	indexer.rw.RLock()
	priIndexes := indexer.priIndexes
	indexer.rw.RUnlock()

	return priIndexes, nil
}

// ----------------------------------------------------

// Create or return a primary index on this keyspace.
func (indexer *indexer) CreatePrimaryIndex(requestId, name string,
	with value.Value) (datastore.PrimaryIndex, errors.Error) {
	return nil, errors.NewError(nil, "unimplemented")
}

// Create or return an index on this keyspace.
func (indexer *indexer) CreateIndex(
	requestId, name string, seekKey, rangeKey expression.Expressions,
	where expression.Expression, with value.Value) (
	datastore.Index, errors.Error) {
	return nil, errors.NewError(nil, "unimplemented")
}

// Build indexes that were deferred at creation.
func (indexer *indexer) BuildIndexes(
	requestId string,
	name ...string) errors.Error {
	return nil // TODO - anything to do here / return unimplemented?
}

// ------------------------------------------------

// Refresh list of indexes from metadata.
func (indexer *indexer) Refresh() errors.Error {
	return indexer.maybeRefresh(true)
}

// Refresh list of indexes from metadata if REFRESH_DURATION_MSEC was
// reached or if force'd.
func (indexer *indexer) maybeRefresh(force bool) (err errors.Error) {
	dur := time.Duration(REFRESH_DURATION_MSEC) * time.Millisecond
	now := time.Now()

	logging.Infof("fts indexer.maybeRefresh indexer: %+v, force: %v",
		indexer, force)

	indexer.rw.Lock()
	if force || indexer.lastRefreshStartTime.IsZero() ||
		now.Sub(indexer.lastRefreshStartTime) > dur {
		force = true

		indexer.lastRefreshStartTime = now
	}
	indexer.rw.Unlock()

	if !force {
		return nil
	}

	mapIndexesById, err := indexer.refresh()
	if err != nil {
		return err
	}

	// Process the mapIndexesById into read-friendly format.

	indexNum := len(mapIndexesById)

	indexIds := make([]string, 0, indexNum)
	indexNames := make([]string, 0, indexNum)

	allIndexes := make([]datastore.Index, 0, indexNum)
	priIndexes := make([]datastore.PrimaryIndex, 0, indexNum)

	mapIndexesByName := map[string]*index{}

	for indexId, index := range mapIndexesById {
		indexIds = append(indexIds, indexId)
		indexNames = append(indexNames, index.Name())

		allIndexes = append(allIndexes, index)

		// if index.isPrimary {
		//     priIndexes = append(priIndexes, index)
		// }

		mapIndexesByName[index.Name()] = index
	}

	indexer.rw.Lock()
	indexer.indexIds = indexIds
	indexer.indexNames = indexNames
	indexer.allIndexes = allIndexes
	indexer.priIndexes = priIndexes
	indexer.mapIndexesById = mapIndexesById
	indexer.mapIndexesByName = mapIndexesByName
	indexer.rw.Unlock()

	return nil
}

func (indexer *indexer) refresh() (map[string]*index, errors.Error) {
	ftsEndpoints, err := indexer.ftsEndpoints()
	if err != nil {
		logging.Warnf("indexer.refresh ftsEndpoints, err: %v", err)

		return nil, errors.NewError(err, "indexer.refresh ftsEndpoints")
	}

	if len(ftsEndpoints) <= 0 {
		return nil, nil
	}

	for i := 0; i < len(ftsEndpoints); i++ {
		x := (int(indexer.startTime.UnixNano()) + i) % len(ftsEndpoints)

		indexDefs, err := indexer.retrieveIndexDefs(ftsEndpoints[x])
		if err == nil {
			return indexer.convertIndexDefs(indexDefs), nil
		} else {
			logging.Warnf("indexer.refresh retrieveIndexDefs, ftsEndpoints: %s,"+
				" indexer: %v, err: %v", ftsEndpoints[x], indexer, err)
		}
	}

	// TODO: If all ftsEndpoints error'ed, then log / propagate error?

	return nil, nil
}

// ------------------------------------------------

// Returns array of FTS endpoint strings that look like
// "http(s)://HOST:PORT".
func (indexer *indexer) ftsEndpoints() (rv []string, err error) {
	user, pswd := "", "" // Forces usage of auth manager.
	user, pswd = "Administrator", "password" // TODO.

	buckets, err := indexer.cluster.Manager(user, pswd).GetBuckets()
	if err != nil {
		return nil, err
	}
	if len(buckets) <= 0 {
		return nil, nil
	}

	bucket, err := indexer.cluster.OpenBucket(buckets[0].Name, "")
	if err != nil {
		return nil, err
	}

	ioRouter := bucket.IoRouter()
	if ioRouter != nil {
		rv = ioRouter.FtsEps()
	}

	bucket.Close()

	return rv, nil
}

// ------------------------------------------------

// Retrieve the index definitions from an FTS endpoint.
func (indexer *indexer) retrieveIndexDefs(ftsEndpoint string) (
	*cbgt.IndexDefs, error) {
	ftsEndpoint = strings.Replace(ftsEndpoint,
		"http://", "http://Administrator:password@", 1) // TODO.

	resp, err := HttpGet(ftsEndpoint + "/api/index") // TODO: Auth.
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("retrieveIndexDefs resp: %#v", resp)
	}

	bodyBuf, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var body struct {
		IndexDefs *cbgt.IndexDefs `json:"indexDefs"`
		Status    string          `json:"status"`
	}

	err = json.Unmarshal(bodyBuf, &body)
	if err != nil {
		return nil, err
	}

	if body.Status != "ok" || body.IndexDefs == nil {
		return nil, fmt.Errorf("retrieveIndexDefs status error,"+
			" body: %+v, bodyBuf: %s", body, bodyBuf)
	}

	return body.IndexDefs, nil
}

// ------------------------------------------------

// Convert an FTS index definitions into a map[n1qlIndexId]*index.
func (indexer *indexer) convertIndexDefs(
	indexDefs *cbgt.IndexDefs) map[string]*index {
	logging.Infof("FTS convertIndexDefs indexDefs: %+v", indexDefs)

	rv := map[string]*index{}

	defer logging.Infof("FTS convertIndexDefs rv: %+v", rv)

OUTER:
	for _, indexDef := range indexDefs.IndexDefs {
		if indexDef.Type != "fulltext-index" {
			logging.Infof("FTS convertIndexDefs SKIP indexDef: %+v,"+
				" not a fulltext-index", indexDef)
			continue
		}

		if indexDef.SourceType != "couchbase" &&
			indexDef.SourceType != "couchbase-dcp" {
			logging.Infof("FTS convertIndexDefs SKIP indexDef: %+v,"+
				" not couchbase/couchbase-dcp SourceType", indexDef)
			continue
		}

		if indexDef.SourceName != indexer.keyspace {
			logging.Infof("FTS convertIndexDefs SKIP indexDef: %+v,"+
				" SourceName != keyspace: %s", indexDef, indexer.keyspace)
			continue
		}

		// TODO: Need to match indexDef.SourceUUID?

		bp := cbft.NewBleveParams()

		err := json.Unmarshal([]byte(indexDef.Params), bp)
		if err != nil {
			logging.Infof("FTS convertIndexDefs SKIP indexDef: %+v,"+
				" json unmarshal indexDef.Params, err: %v", indexDef, err)
			continue
		}

		if bp.DocConfig.Mode != "type_field" {
			logging.Infof("FTS convertIndexDefs SKIP indexDef: %+v,"+
				" wrong DocConfig.Mode", indexDef)
			continue
		}

		typeField := bp.DocConfig.TypeField
		if typeField == "" {
			logging.Infof("FTS convertIndexDefs SKIP indexDef: %+v,"+
				" wrong DocConfig.TypeField", indexDef)
			continue
		}

		bm := bp.Mapping

		if bm.IndexDynamic { // TODO: Handle IndexDynamic one day.
			logging.Infof("FTS convertIndexDefs SKIP indexDef: %+v,"+
				" IndexDynamic", indexDef)
			continue
		}

		// Keyed by field path (ex: "name", "address.geo.lat").
		fieldPathIndexFields := map[string][]*indexField{}

		if len(bm.TypeMapping) > 0 {
			// If we have type mappings, then we can't support any
			// default mapping.
			//
			if bm.TypeMapping[bm.DefaultType] != nil {
				logging.Infof("FTS convertIndexDefs SKIP indexDef: %+v,"+
					" TypeMapping[bm.DefaultType] non-nil", indexDef)
				continue
			}

			if bm.DefaultMapping != nil &&
				bm.DefaultMapping.Enabled {
				if bm.DefaultMapping.Dynamic ||
					len(bm.DefaultMapping.Properties) > 0 ||
					len(bm.DefaultMapping.Fields) > 0 {
					logging.Infof("FTS convertIndexDefs SKIP indexDef: %+v,"+
						" default mapping exists when type mappings exist", indexDef)
					continue // TODO: Handle default mapping one day.
				}
			}

			for t, m := range bm.TypeMapping {
				allowed := indexer.convertDocMap(t, false, "", "",
					m, fieldPathIndexFields)
				if !allowed {
					logging.Infof("FTS convertIndexDefs SKIP indexDef: %+v,"+
						" convertDocMap disallowed for type: %v, m: %v",
						indexDef, t, m)
					continue OUTER
				}
			}
		} else {
			// If we have no type mappings, then we have to have an
			// enabled, non-dynamic, default mapping.
			//
			if bm.DefaultMapping == nil ||
				bm.DefaultMapping.Enabled == false ||
				bm.DefaultMapping.Dynamic {
				continue
			}

			if len(bm.DefaultMapping.Properties) <= 0 &&
				len(bm.DefaultMapping.Fields) <= 0 {
				continue
			}

			defaultDocMap := bm.TypeMapping[bm.DefaultType]
			if defaultDocMap == nil {
				defaultDocMap = bm.DefaultMapping
			}
			if defaultDocMap == nil {
				continue
			}

			allowed := indexer.convertDocMap("", true, "", "",
				defaultDocMap, fieldPathIndexFields)
			if !allowed {
				continue
			}
		}

		// Filter out any fieldPath that has uses a non-"keyword"
		// analyzer.
		//
		for fieldPath, indexFields := range fieldPathIndexFields {
			for _, indexField := range indexFields {
				if indexField.field.Analyzer != "keyword" {
					delete(fieldPathIndexFields, fieldPath)
				}
			}
		}

		// Generate index metadata for each fieldPath.
		//
		for fieldPath, indexFields := range fieldPathIndexFields {
			fieldPathS := fmt.Sprintf("`%s`.`%s`", indexer.keyspace, fieldPath)

			rangeKeyExpr, err := parser.Parse(fieldPathS)
			if err != nil {
				logging.Infof("FTS convertIndexDefs SKIP indexDef: %+v,"+
					" parse fieldPath: %v, err: %v",
					indexDef, fieldPath, err)
				continue OUTER
			}

			indexName := "__FTS__/" + indexDef.Name + "/" + fieldPath

			index := &index{
				indexer:       indexer,
				id:            indexName + "/" + indexDef.UUID,
				name:          indexName,
				indexDef:      indexDef,
				typeField:     typeField,
				fieldPath:     fieldPath,
				indexFields:   indexFields,
				seekKeyExprs:  expression.Expressions{},
				rangeKeyExprs: expression.Expressions{rangeKeyExpr},
			}

			conditionExprs := make([]string, 0, len(indexFields))
			for _, indexField := range indexFields {
				if !indexField.isDefault {
					conditionExprs = append(conditionExprs,
						fmt.Sprintf("`%s` = %q", typeField, indexField.docType))
				}
			}

			if len(conditionExprs) > 0 {
				conditionExpr, err :=
					parser.Parse(strings.Join(conditionExprs, " OR "))
				if err != nil {
					logging.Infof("FTS convertIndexDefs SKIP indexDef: %+v,"+
						" parse conditionExprs: %+v, err: %v",
						indexDef, conditionExprs, err)
					continue OUTER
				}

				index.conditionExpr = conditionExpr
			}

			rv[index.id] = index
		}
	}

	return rv
}

// ------------------------------------------------

// convertDocMap() recursively processes the parts of a
// bleve.DocumentMapping into one or more N1QL datastore.Index
// metadata instances.
//
// The docType might be nil when it's the default docMapping.
//
// Returns true if the conversion should continue for the entire
// indexDef, or false if conversion should stop (such as due to
// dynamic mapping).
//
// TODO: We currently ignore the _all / IncludeInAll feature.
func (indexer *indexer) convertDocMap(docType string, isDefault bool,
	docMapPath, docMapName string, docMap *bleve.DocumentMapping,
	rv map[string][]*indexField) bool {
	if docMap == nil || !docMap.Enabled {
		return true
	}

	if docMap.Dynamic {
		// Any dynamic seen anywhere means disallow the conversion of
		// the indexDef.
		//
		// TODO: One day figure out how to convert dynamic FTS indexes.
		//
		return false
	}

	for _, field := range docMap.Fields {
		if field.Index == false {
			continue
		}

		if docMapName != "" && docMapName != field.Name {
			return false
		}

		rv[docMapPath] = append(rv[docMapPath], &indexField{
			docType:   docType,
			isDefault: isDefault,
			field:     field,
		})
	}

	for childName, childMap := range docMap.Properties {
		childPath := docMapPath
		if len(childPath) > 0 {
			childPath += "."
		}
		childPath += childName

		allowed := indexer.convertDocMap(docType, isDefault,
			childPath, childName, childMap, rv)
		if !allowed {
			return false
		}
	}

	return true
}

// ------------------------------------------------

// Set log level for in-process logging.
func (indexer *indexer) SetLogLevel(level logging.Level) {
	// TODO.
}

// ------------------------------------------------

// Id of the keyspace to which this index belongs.
func (i *index) KeyspaceId() string {
	return i.indexer.keyspace
}

// Id of this index.
func (i *index) Id() string {
	return i.name
}

// Name of this index.
func (i *index) Name() string {
	return i.name
}

// Type of this index.
func (i *index) Type() datastore.IndexType {
	return datastore.IndexType(INDEXER_PROVIDER_NAME)
}

// Equality keys.
func (i *index) SeekKey() expression.Expressions {
	return i.seekKeyExprs
}

// Range keys.
func (i *index) RangeKey() expression.Expressions {
	return i.rangeKeyExprs
}

// Condition, if any.
func (i *index) Condition() expression.Expression {
	return i.conditionExpr
}

// Is this a primary index?
func (i *index) IsPrimary() bool {
	return i.isPrimary
}

// Obtain state of this index.
func (i *index) State() (state datastore.IndexState, msg string, err errors.Error) {
	return datastore.ONLINE, "", nil
}

// Obtain statistics for this index.
func (i *index) Statistics(requestId string, span *datastore.Span) (
	datastore.Statistics, errors.Error) {
	return &indexStatistics{index: i}, nil
}

// Drop / delete this index.
func (i *index) Drop(requestId string) errors.Error {
	return errors.NewError(nil, "unimplemented")
}

// Perform a scan on this index. Distinct and limit are hints.
func (i *index) Scan(requestId string, span *datastore.Span, distinct bool,
	limit int64, cons datastore.ScanConsistency,
	vector timestamp.Vector, conn *datastore.IndexConnection) {
	stopCh := conn.StopChannel()
	entryCh := conn.EntryChannel()

	defer close(entryCh)

	// TODO. handle distinct.
	// TODO. construct bquery and do something with span.
	// TODO. do something with vector and scan consistency.

	// TODO: We currently only support exact equals scan.
	if len(span.Range.Low) != 1 ||
		len(span.Range.High) != 1 ||
		span.Range.Low[0].EquivalentTo(span.Range.High[0]) == false ||
		span.Range.Inclusion != datastore.BOTH {
		conn.Error(errors.NewError(nil,
			"n1fty currently implements only exact match span"))
		return
	}

	bucketPswd := "" // TODO.

	bucket, err := i.indexer.cluster.OpenBucket(i.indexer.keyspace, bucketPswd)
	if err != nil {
		conn.Error(errors.NewError(err, "fts ExecuteSearchQuery OpenBucket"))
		return
	}
	defer bucket.Close()

	term := span.Range.Low[0].String() // Enclosed with double-quotes.
	if term[0] == '"' && term[len(term)-1] == '"' {
		term = term[1: len(term)-1]
	}

	logging.Infof("fts index.Scan, index.id: %#v, requestId: %s, term: %s",
		i.id, requestId, term)

	tquery := gocbCBFT.NewTermQuery(term).Field(i.fieldPath)

	squery := gocb.NewSearchQuery(i.indexDef.Name, tquery)

	limiti := int(limit)
	if limiti > 10000 {
		limiti = 10000 // TODO.
	}
	squery.Limit(limiti)

	sresults, err := bucket.ExecuteSearchQuery(squery)

	logging.Infof("fts bucket.ExecuteSearchQuery, index.id: %#v,"+
		" requestId: %s, term: %s, err: %v",
		i.id, requestId, term, err)

	if err != nil {
		conn.Error(errors.NewError(err,
			fmt.Sprintf("fts ExecuteSearchQuery, err: %v", err)))
		return
	}

	for _, errMsg := range sresults.Errors() {
		conn.Error(errors.NewError(err,
			fmt.Sprintf("fts ExecuteSearchQuery results.Errors, errMsg: %s", errMsg)))
		return
	}

	if sresults.Status().Failed > 0 {
		conn.Error(errors.NewError(nil,
			fmt.Sprintf("fts search failed, status: %v", sresults.Status)))
		return
	}

	for _, hit := range sresults.Hits() {
		select {
		case entryCh <- &datastore.IndexEntry{PrimaryKey: hit.Id}:
			// NO-OP.

		case <-stopCh:
			return
		}
	}
}

// TODO: In order to implement datastore.PrimaryIndex interface.
/*
func (i *index) ScanEntries(requestId string,
	limit int64, cons datastore.ScanConsistency,
	vector timestamp.Vector, conn *datastore.IndexConnection) {
	close(conn.EntryChannel()) // TODO.

	conn.Error(errors.NewError(nil, "unimplemented"))
}
*/

// TODO: In order to implement datastore.SizedIndex interface.
/*
func (i *index) SizeFromStatistics(requestId string) (
	int64, errors.Error) {
	return 0, nil // TODO.
}
*/

// TODO: In order to implement datastore.CountIndex{} interface.
/*
func (i *index) Count(span *datastore.Span,
	cons datastore.ScanConsistency,
	vector timestamp.Vector) (int64, errors.Error) {
	return 0, nil // TODO.
}
*/

// ------------------------------------------------

type indexStatistics struct {
	index *index
}

func (is *indexStatistics) Count() (int64, errors.Error) {
	return 0, nil // TODO.
}

func (is *indexStatistics) Min() (value.Values, errors.Error) {
	return nil, nil // TODO.
}

func (is *indexStatistics) Max() (value.Values, errors.Error) {
	return nil, nil // TODO.
}

func (is *indexStatistics) DistinctCount() (int64, errors.Error) {
	return 0, nil // TODO.
}

func (is *indexStatistics) Bins() ([]datastore.Statistics, errors.Error) {
	return nil, nil // TODO.
}
