/*
Copyright 2019 The Vitess Authors.

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

package wrangler

import (
	"bytes"
	"fmt"
	"html/template"
	"sort"
	"sync"
	"time"

	"context"

	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/vt/concurrency"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/logutil"
	"vitess.io/vitess/go/vt/mysqlctl/tmutils"
	"vitess.io/vitess/go/vt/schema"
	"vitess.io/vitess/go/vt/topo"
	"vitess.io/vitess/go/vt/topo/topoproto"

	tabletmanagerdatapb "vitess.io/vitess/go/vt/proto/tabletmanagerdata"
	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
	vtctldatapb "vitess.io/vitess/go/vt/proto/vtctldata"
)

const (
	// DefaultWaitReplicasTimeout is the default value for waitReplicasTimeout, which is used when calling method CopySchemaShardFromShard.
	DefaultWaitReplicasTimeout = 10 * time.Second
)

// GetSchema uses an RPC to get the schema from a remote tablet
func (wr *Wrangler) GetSchema(ctx context.Context, tabletAlias *topodatapb.TabletAlias, tables, excludeTables []string, includeViews bool) (*tabletmanagerdatapb.SchemaDefinition, error) {
	ti, err := wr.ts.GetTablet(ctx, tabletAlias)
	if err != nil {
		return nil, fmt.Errorf("GetTablet(%v) failed: %v", tabletAlias, err)
	}

	return wr.tmc.GetSchema(ctx, ti.Tablet, tables, excludeTables, includeViews)
}

// helper method to asynchronously diff a schema
func (wr *Wrangler) diffSchema(ctx context.Context, primarySchema *tabletmanagerdatapb.SchemaDefinition, primaryTabletAlias, alias *topodatapb.TabletAlias, excludeTables []string, includeViews bool, wg *sync.WaitGroup, er concurrency.ErrorRecorder) {
	defer wg.Done()
	log.Infof("Gathering schema for %v", topoproto.TabletAliasString(alias))
	replicaSchema, err := wr.GetSchema(ctx, alias, nil, excludeTables, includeViews)
	if err != nil {
		er.RecordError(fmt.Errorf("GetSchema(%v, nil, %v, %v) failed: %v", alias, excludeTables, includeViews, err))
		return
	}

	log.Infof("Diffing schema for %v", topoproto.TabletAliasString(alias))
	tmutils.DiffSchema(topoproto.TabletAliasString(primaryTabletAlias), primarySchema, topoproto.TabletAliasString(alias), replicaSchema, er)
}

// ValidateSchemaShard will diff the schema from all the tablets in the shard.
func (wr *Wrangler) ValidateSchemaShard(ctx context.Context, keyspace, shard string, excludeTables []string, includeViews bool, includeVSchema bool) error {
	si, err := wr.ts.GetShard(ctx, keyspace, shard)
	if err != nil {
		return fmt.Errorf("GetShard(%v, %v) failed: %v", keyspace, shard, err)
	}

	// get schema from the primary, or error
	if !si.HasPrimary() {
		return fmt.Errorf("no primary in shard %v/%v", keyspace, shard)
	}
	log.Infof("Gathering schema for primary %v", topoproto.TabletAliasString(si.PrimaryAlias))
	primarySchema, err := wr.GetSchema(ctx, si.PrimaryAlias, nil, excludeTables, includeViews)
	if err != nil {
		return fmt.Errorf("GetSchema(%v, nil, %v, %v) failed: %v", si.PrimaryAlias, excludeTables, includeViews, err)
	}

	if includeVSchema {
		err := wr.ValidateVSchema(ctx, keyspace, []string{shard}, excludeTables, includeViews)
		if err != nil {
			return err
		}
	}

	// read all the aliases in the shard, that is all tablets that are
	// replicating from the primary
	aliases, err := wr.ts.FindAllTabletAliasesInShard(ctx, keyspace, shard)
	if err != nil {
		return fmt.Errorf("FindAllTabletAliasesInShard(%v, %v) failed: %v", keyspace, shard, err)
	}

	// then diff with all replicas
	er := concurrency.AllErrorRecorder{}
	wg := sync.WaitGroup{}
	for _, alias := range aliases {
		if topoproto.TabletAliasEqual(alias, si.PrimaryAlias) {
			continue
		}

		wg.Add(1)
		go wr.diffSchema(ctx, primarySchema, si.PrimaryAlias, alias, excludeTables, includeViews, &wg, &er)
	}
	wg.Wait()
	if er.HasErrors() {
		return fmt.Errorf("schema diffs: %v", er.Error().Error())
	}
	return nil
}

// ValidateSchemaKeyspace will diff the schema from all the tablets in
// the keyspace.
func (wr *Wrangler) ValidateSchemaKeyspace(ctx context.Context, keyspace string, excludeTables []string, includeViews, skipNoPrimary bool, includeVSchema bool) error {
	// find all the shards
	shards, err := wr.ts.GetShardNames(ctx, keyspace)
	if err != nil {
		return fmt.Errorf("GetShardNames(%v) failed: %v", keyspace, err)
	}

	// corner cases
	if len(shards) == 0 {
		return fmt.Errorf("no shards in keyspace %v", keyspace)
	}
	sort.Strings(shards)
	if len(shards) == 1 {
		return wr.ValidateSchemaShard(ctx, keyspace, shards[0], excludeTables, includeViews, includeVSchema)
	}

	var referenceSchema *tabletmanagerdatapb.SchemaDefinition
	var referenceAlias *topodatapb.TabletAlias

	// then diff with all other tablets everywhere
	er := concurrency.AllErrorRecorder{}
	wg := sync.WaitGroup{}

	// If we are checking against the vschema then all shards
	// should just be validated individually against it
	if includeVSchema {
		err := wr.ValidateVSchema(ctx, keyspace, shards, excludeTables, includeViews)
		if err != nil {
			return err
		}
	}

	// then diffs all tablets in the other shards
	for _, shard := range shards[0:] {
		si, err := wr.ts.GetShard(ctx, keyspace, shard)
		if err != nil {
			er.RecordError(fmt.Errorf("GetShard(%v, %v) failed: %v", keyspace, shard, err))
			continue
		}

		if !si.HasPrimary() {
			if !skipNoPrimary {
				er.RecordError(fmt.Errorf("no primary in shard %v/%v", keyspace, shard))
			}
			continue
		}

		if referenceSchema == nil {
			referenceAlias = si.PrimaryAlias
			log.Infof("Gathering schema for reference primary %v", topoproto.TabletAliasString(referenceAlias))
			referenceSchema, err = wr.GetSchema(ctx, referenceAlias, nil, excludeTables, includeViews)
			if err != nil {
				return fmt.Errorf("GetSchema(%v, nil, %v, %v) failed: %v", referenceAlias, excludeTables, includeViews, err)
			}
		}

		aliases, err := wr.ts.FindAllTabletAliasesInShard(ctx, keyspace, shard)
		if err != nil {
			er.RecordError(fmt.Errorf("FindAllTabletAliasesInShard(%v, %v) failed: %v", keyspace, shard, err))
			continue
		}

		for _, alias := range aliases {
			// Don't diff schemas for self
			if referenceAlias == alias {
				continue
			}
			wg.Add(1)
			go wr.diffSchema(ctx, referenceSchema, referenceAlias, alias, excludeTables, includeViews, &wg, &er)
		}
	}
	wg.Wait()
	if er.HasErrors() {
		return fmt.Errorf("schema diffs: %v", er.Error().Error())
	}
	return nil
}

// ValidateVSchema compares the schema of each primary tablet in "keyspace/shards..." to the vschema and errs if there are differences
func (wr *Wrangler) ValidateVSchema(ctx context.Context, keyspace string, shards []string, excludeTables []string, includeViews bool) error {
	vschm, err := wr.ts.GetVSchema(ctx, keyspace)
	if err != nil {
		return fmt.Errorf("GetVSchema(%s) failed: %v", keyspace, err)
	}

	shardFailures := concurrency.AllErrorRecorder{}
	var wg sync.WaitGroup
	wg.Add(len(shards))

	for _, shard := range shards {
		go func(shard string) {
			defer wg.Done()
			notFoundTables := []string{}
			si, err := wr.ts.GetShard(ctx, keyspace, shard)
			if err != nil {
				shardFailures.RecordError(fmt.Errorf("GetShard(%v, %v) failed: %v", keyspace, shard, err))
				return
			}
			primarySchema, err := wr.GetSchema(ctx, si.PrimaryAlias, nil, excludeTables, includeViews)
			if err != nil {
				shardFailures.RecordError(fmt.Errorf("GetSchema(%s, nil, %v, %v) (%v/%v) failed: %v", si.PrimaryAlias.String(),
					excludeTables, includeViews, keyspace, shard, err,
				))
				return
			}
			for _, tableDef := range primarySchema.TableDefinitions {
				if _, ok := vschm.Tables[tableDef.Name]; !ok {
					if schema.IsInternalOperationTableName(tableDef.Name) {
						log.Infof("found internal table %s, ignoring in vschema validation", tableDef.Name)
					} else {
						notFoundTables = append(notFoundTables, tableDef.Name)
					}
				}
			}
			if len(notFoundTables) > 0 {
				shardFailure := fmt.Errorf("%v/%v has tables that are not in the vschema: %v", keyspace, shard, notFoundTables)
				shardFailures.RecordError(shardFailure)
			}
		}(shard)
	}
	wg.Wait()
	if shardFailures.HasErrors() {
		return fmt.Errorf("ValidateVSchema(%v, %v, %v, %v) failed: %v", keyspace, shards, excludeTables, includeViews, shardFailures.Error().Error())
	}
	return nil
}

// PreflightSchema will try a schema change on the remote tablet.
func (wr *Wrangler) PreflightSchema(ctx context.Context, tabletAlias *topodatapb.TabletAlias, changes []string) ([]*tabletmanagerdatapb.SchemaChangeResult, error) {
	ti, err := wr.ts.GetTablet(ctx, tabletAlias)
	if err != nil {
		return nil, fmt.Errorf("GetTablet(%v) failed: %v", tabletAlias, err)
	}
	return wr.tmc.PreflightSchema(ctx, ti.Tablet, changes)
}

// CopySchemaShardFromShard copies the schema from a source shard to the specified destination shard.
// For both source and destination it picks the primary tablet. See also CopySchemaShard.
func (wr *Wrangler) CopySchemaShardFromShard(ctx context.Context, tables, excludeTables []string, includeViews bool, sourceKeyspace, sourceShard, destKeyspace, destShard string, waitReplicasTimeout time.Duration, skipVerify bool) error {
	sourceShardInfo, err := wr.ts.GetShard(ctx, sourceKeyspace, sourceShard)
	if err != nil {
		return fmt.Errorf("GetShard(%v, %v) failed: %v", sourceKeyspace, sourceShard, err)
	}
	if sourceShardInfo.PrimaryAlias == nil {
		return fmt.Errorf("no primary in shard record %v/%v. Consider running 'vtctl InitShardPrimary' in case of a new shard or reparenting the shard to fix the topology data, or providing a non-primary tablet alias", sourceKeyspace, sourceShard)
	}

	return wr.CopySchemaShard(ctx, sourceShardInfo.PrimaryAlias, tables, excludeTables, includeViews, destKeyspace, destShard, waitReplicasTimeout, skipVerify)
}

// CopySchemaShard copies the schema from a source tablet to the
// specified shard.  The schema is applied directly on the primary of
// the destination shard, and is propagated to the replicas through
// binlogs.
func (wr *Wrangler) CopySchemaShard(ctx context.Context, sourceTabletAlias *topodatapb.TabletAlias, tables, excludeTables []string, includeViews bool, destKeyspace, destShard string, waitReplicasTimeout time.Duration, skipVerify bool) error {
	destShardInfo, err := wr.ts.GetShard(ctx, destKeyspace, destShard)
	if err != nil {
		return fmt.Errorf("GetShard(%v, %v) failed: %v", destKeyspace, destShard, err)
	}

	if destShardInfo.PrimaryAlias == nil {
		return fmt.Errorf("no primary in shard record %v/%v. Consider running 'vtctl InitShardPrimary' in case of a new shard or reparenting the shard to fix the topology data", destKeyspace, destShard)
	}

	err = wr.copyShardMetadata(ctx, sourceTabletAlias, destShardInfo.PrimaryAlias)
	if err != nil {
		return fmt.Errorf("copyShardMetadata(%v, %v) failed: %v", sourceTabletAlias, destShardInfo.PrimaryAlias, err)
	}

	diffs, err := wr.compareSchemas(ctx, sourceTabletAlias, destShardInfo.PrimaryAlias, tables, excludeTables, includeViews)
	if err != nil {
		return fmt.Errorf("CopySchemaShard failed because schemas could not be compared initially: %v", err)
	}
	if diffs == nil {
		// Return early because dest has already the same schema as source.
		return nil
	}

	sourceSd, err := wr.GetSchema(ctx, sourceTabletAlias, tables, excludeTables, includeViews)
	if err != nil {
		return fmt.Errorf("GetSchema(%v, %v, %v, %v) failed: %v", sourceTabletAlias, tables, excludeTables, includeViews, err)
	}
	createSQL := tmutils.SchemaDefinitionToSQLStrings(sourceSd)
	destTabletInfo, err := wr.ts.GetTablet(ctx, destShardInfo.PrimaryAlias)
	if err != nil {
		return fmt.Errorf("GetTablet(%v) failed: %v", destShardInfo.PrimaryAlias, err)
	}
	for i, sqlLine := range createSQL {
		err = wr.applySQLShard(ctx, destTabletInfo, sqlLine, i == len(createSQL)-1)
		if err != nil {
			return fmt.Errorf("creating a table failed."+
				" Most likely some tables already exist on the destination and differ from the source."+
				" Please remove all to be copied tables from the destination manually and run this command again."+
				" Full error: %v", err)
		}
	}

	// Remember the replication position after all the above were applied.
	destPrimaryPos, err := wr.tmc.MasterPosition(ctx, destTabletInfo.Tablet)
	if err != nil {
		return fmt.Errorf("CopySchemaShard: can't get replication position after schema applied: %v", err)
	}

	// Although the copy was successful, we have to verify it to catch the case
	// where the database already existed on the destination, but with different
	// options e.g. a different character set.
	// In that case, MySQL would have skipped our CREATE DATABASE IF NOT EXISTS
	// statement. We want to fail early in this case because vtworker SplitDiff
	// fails in case of such an inconsistency as well.
	if !skipVerify {
		diffs, err = wr.compareSchemas(ctx, sourceTabletAlias, destShardInfo.PrimaryAlias, tables, excludeTables, includeViews)
		if err != nil {
			return fmt.Errorf("CopySchemaShard failed because schemas could not be compared finally: %v", err)
		}
		if diffs != nil {
			return fmt.Errorf("CopySchemaShard was not successful because the schemas between the two tablets %v and %v differ: %v", sourceTabletAlias, destShardInfo.PrimaryAlias, diffs)
		}
	}

	// Notify Replicass to reload schema. This is best-effort.
	reloadCtx, cancel := context.WithTimeout(ctx, waitReplicasTimeout)
	defer cancel()
	resp, err := wr.VtctldServer().ReloadSchemaShard(reloadCtx, &vtctldatapb.ReloadSchemaShardRequest{
		Keyspace:       destKeyspace,
		Shard:          destShard,
		WaitPosition:   destPrimaryPos,
		Concurrency:    10,
		IncludePrimary: true,
	})
	if resp != nil {
		for _, e := range resp.Events {
			logutil.LogEvent(wr.Logger(), e)
		}
	}
	return err
}

// copyShardMetadata copies contents of _vt.shard_metadata table from the source
// tablet to the destination tablet. It's assumed that destination tablet is a
// primary and binlogging is not turned off when INSERT statements are executed.
func (wr *Wrangler) copyShardMetadata(ctx context.Context, srcTabletAlias *topodatapb.TabletAlias, destTabletAlias *topodatapb.TabletAlias) error {
	sql := "SELECT 1 FROM information_schema.tables WHERE table_schema = '_vt' AND table_name = 'shard_metadata'"
	presenceResult, err := wr.ExecuteFetchAsDba(ctx, srcTabletAlias, sql, 1, false, false)
	if err != nil {
		return fmt.Errorf("ExecuteFetchAsDba(%v, %v, 1, false, false) failed: %v", srcTabletAlias, sql, err)
	}
	if len(presenceResult.Rows) == 0 {
		log.Infof("_vt.shard_metadata doesn't exist on the source tablet %v, skipping its copy.", topoproto.TabletAliasString(srcTabletAlias))
		return nil
	}

	// TODO: 100 may be too low here for row limit
	sql = "SELECT db_name, name, value FROM _vt.shard_metadata"
	dataProto, err := wr.ExecuteFetchAsDba(ctx, srcTabletAlias, sql, 100, false, false)
	if err != nil {
		return fmt.Errorf("ExecuteFetchAsDba(%v, %v, 100, false, false) failed: %v", srcTabletAlias, sql, err)
	}
	data := sqltypes.Proto3ToResult(dataProto)
	for _, row := range data.Rows {
		dbName := row[0]
		name := row[1]
		value := row[2]
		queryBuf := bytes.Buffer{}
		queryBuf.WriteString("INSERT INTO _vt.shard_metadata (db_name, name, value) VALUES (")
		dbName.EncodeSQL(&queryBuf)
		queryBuf.WriteByte(',')
		name.EncodeSQL(&queryBuf)
		queryBuf.WriteByte(',')
		value.EncodeSQL(&queryBuf)
		queryBuf.WriteString(") ON DUPLICATE KEY UPDATE value = ")
		value.EncodeSQL(&queryBuf)

		_, err := wr.ExecuteFetchAsDba(ctx, destTabletAlias, queryBuf.String(), 0, false, false)
		if err != nil {
			return fmt.Errorf("ExecuteFetchAsDba(%v, %v, 0, false, false) failed: %v", destTabletAlias, queryBuf.String(), err)
		}
	}
	return nil
}

// compareSchemas returns nil if the schema of the two tablets referenced by
// "sourceAlias" and "destAlias" are identical. Otherwise, the difference is
// returned as []string.
func (wr *Wrangler) compareSchemas(ctx context.Context, sourceAlias, destAlias *topodatapb.TabletAlias, tables, excludeTables []string, includeViews bool) ([]string, error) {
	sourceSd, err := wr.GetSchema(ctx, sourceAlias, tables, excludeTables, includeViews)
	if err != nil {
		return nil, fmt.Errorf("failed to get schema from tablet %v. err: %v", sourceAlias, err)
	}
	destSd, err := wr.GetSchema(ctx, destAlias, tables, excludeTables, includeViews)
	if err != nil {
		return nil, fmt.Errorf("failed to get schema from tablet %v. err: %v", destAlias, err)
	}
	return tmutils.DiffSchemaToArray("source", sourceSd, "dest", destSd), nil
}

// applySQLShard applies a given SQL change on a given tablet alias. It allows executing arbitrary
// SQL statements, but doesn't return any results, so it's only useful for SQL statements
// that would be run for their effects (e.g., CREATE).
// It works by applying the SQL statement on the shard's primary tablet with replication turned on.
// Thus it should be used only for changes that can be applied on a live instance without causing issues;
// it shouldn't be used for anything that will require a pivot.
// The SQL statement string is expected to have {{.DatabaseName}} in place of the actual db name.
func (wr *Wrangler) applySQLShard(ctx context.Context, tabletInfo *topo.TabletInfo, change string, reloadSchema bool) error {
	filledChange, err := fillStringTemplate(change, map[string]string{"DatabaseName": tabletInfo.DbName()})
	if err != nil {
		return fmt.Errorf("fillStringTemplate failed: %v", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Need to make sure that we enable binlog, since we're only applying the statement on primaries.
	_, err = wr.tmc.ExecuteFetchAsDba(ctx, tabletInfo.Tablet, false, []byte(filledChange), 0, false, reloadSchema)
	return err
}

// fillStringTemplate returns the string template filled
func fillStringTemplate(tmpl string, vars interface{}) (string, error) {
	myTemplate := template.Must(template.New("").Parse(tmpl))
	data := new(bytes.Buffer)
	if err := myTemplate.Execute(data, vars); err != nil {
		return "", err
	}
	return data.String(), nil
}
