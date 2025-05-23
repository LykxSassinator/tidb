// Copyright 2023 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package checkpoints_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/br/pkg/version/build"
	"github.com/pingcap/tidb/pkg/lightning/checkpoints"
	"github.com/pingcap/tidb/pkg/lightning/mydump"
	"github.com/pingcap/tidb/pkg/lightning/verification"
	"github.com/pingcap/tidb/pkg/meta/model"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/stretchr/testify/require"
)

type cpSQLSuite struct {
	db   *sql.DB
	mock sqlmock.Sqlmock
	cpdb *checkpoints.MySQLCheckpointsDB
}

func newCPSQLSuite(t *testing.T) *cpSQLSuite {
	var s cpSQLSuite
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	s.db = db
	s.mock = mock

	// 1. create the checkpoints database.
	s.mock.
		ExpectExec("CREATE DATABASE IF NOT EXISTS `mock-schema`").
		WillReturnResult(sqlmock.NewResult(1, 1))
	s.mock.
		ExpectExec("CREATE TABLE IF NOT EXISTS `mock-schema`\\.`task_v\\d+` .+").
		WillReturnResult(sqlmock.NewResult(2, 1))
	s.mock.
		ExpectExec("CREATE TABLE IF NOT EXISTS `mock-schema`\\.`table_v\\d+` .+").
		WillReturnResult(sqlmock.NewResult(3, 1))
	s.mock.
		ExpectExec("CREATE TABLE IF NOT EXISTS `mock-schema`\\.`engine_v\\d+` .+").
		WillReturnResult(sqlmock.NewResult(4, 1))
	s.mock.
		ExpectExec("CREATE TABLE IF NOT EXISTS `mock-schema`\\.`chunk_v\\d+` .+").
		WillReturnResult(sqlmock.NewResult(5, 1))

	cpdb, err := checkpoints.NewMySQLCheckpointsDB(context.Background(), s.db, "mock-schema")
	require.NoError(t, err)
	require.Nil(t, s.mock.ExpectationsWereMet())
	s.cpdb = cpdb
	t.Cleanup(func() {
		s.mock.ExpectClose()
		require.Nil(t, s.cpdb.Close())
		require.Nil(t, s.mock.ExpectationsWereMet())
	})
	return &s
}

func TestNormalOperations(t *testing.T) {
	ctx := context.Background()
	s := newCPSQLSuite(t)
	cpdb := s.cpdb
	s.mock.ExpectBegin()
	initializeStmt := s.mock.ExpectPrepare(
		"REPLACE INTO `mock-schema`\\.`task_v\\d+`")
	initializeStmt.ExpectExec().
		WithArgs(123, "/data", "local", "127.0.0.1:8287", "127.0.0.1", 4000, "127.0.0.1:2379", "/tmp/sorted-kv", build.ReleaseVersion).
		WillReturnResult(sqlmock.NewResult(6, 1))
	initializeStmt = s.mock.
		ExpectPrepare("INSERT INTO `mock-schema`\\.`table_v\\d+`")
	initializeStmt.ExpectExec().
		WithArgs(123, "`db1`.`t2`", sqlmock.AnyArg(), int64(2), []byte("")).
		WillReturnResult(sqlmock.NewResult(8, 1))
	s.mock.ExpectCommit()

	s.mock.MatchExpectationsInOrder(false)
	cfg := newTestConfig()
	err := cpdb.Initialize(ctx, cfg, map[string]*checkpoints.TidbDBInfo{
		"db1": {
			Name: "db1",
			Tables: map[string]*checkpoints.TidbTableInfo{
				"t2": {
					Name: "t2",
					ID:   2,
					Desired: &model.TableInfo{
						Name: ast.NewCIStr("t2"),
					},
				},
			},
		},
	})
	s.mock.MatchExpectationsInOrder(true)
	require.NoError(t, err)
	require.Nil(t, s.mock.ExpectationsWereMet())

	s.mock.ExpectBegin()
	s.mock.
		ExpectQuery("SELECT .+ FROM `mock-schema`\\.`engine_v\\d+`").
		WithArgs("`db1`.`t2`").
		WillReturnRows(
			sqlmock.NewRows([]string{"engine_id", "status"}).
				AddRow(0, 120).
				AddRow(-1, 30),
		)
	s.mock.
		ExpectQuery("SELECT (?s:.+) FROM `mock-schema`\\.`chunk_v\\d+`").
		WithArgs("`db1`.`t2`").
		WillReturnRows(
			sqlmock.NewRows([]string{
				"engine_id", "path", "offset", "type", "compression", "sort_key", "file_size", "columns",
				"pos", "real_pos", "end_offset", "prev_rowid_max", "rowid_max",
				"kvc_bytes", "kvc_kvs", "kvc_checksum", "unix_timestamp(create_time)",
			}).
				AddRow(
					0, "/tmp/path/1.sql", 0, mydump.SourceTypeSQL, 0, "", 123, "[]",
					55904, 55902, 102400, 681, 5000,
					4491, 586, 486070148917, 1234567894,
				),
		)
	s.mock.
		ExpectQuery("SELECT .+ FROM `mock-schema`\\.`table_v\\d+`").
		WithArgs("`db1`.`t2`").
		WillReturnRows(sqlmock.NewRows([]string{
			"status", "table_id", "table_info", "kv_bytes", "kv_kvs", "kv_checksum",
			"auto_rand_base", "auto_incr_base", "auto_row_id_base"}).
			AddRow(60, int64(2), nil, uint64(4492), uint64(686), uint64(486070148910), 132861, 132862, 132863))
	s.mock.ExpectCommit()

	cp, err := cpdb.Get(ctx, "`db1`.`t2`")
	require.Nil(t, err)
	require.Equal(t, &checkpoints.TableCheckpoint{
		Status:        checkpoints.CheckpointStatusAllWritten,
		AutoRandBase:  132861,
		AutoIncrBase:  132862,
		AutoRowIDBase: 132863,
		TableID:       int64(2),
		TableInfo:     nil,
		Engines: map[int32]*checkpoints.EngineCheckpoint{
			-1: {Status: checkpoints.CheckpointStatusLoaded},
			0: {
				Status: checkpoints.CheckpointStatusImported,
				Chunks: []*checkpoints.ChunkCheckpoint{{
					Key: checkpoints.ChunkCheckpointKey{
						Path:   "/tmp/path/1.sql",
						Offset: 0,
					},
					FileMeta: mydump.SourceFileMeta{
						Path:     "/tmp/path/1.sql",
						Type:     mydump.SourceTypeSQL,
						FileSize: 123,
					},
					ColumnPermutation: []int{},
					Chunk: mydump.Chunk{
						Offset:       55904,
						RealOffset:   55902,
						EndOffset:    102400,
						PrevRowIDMax: 681,
						RowIDMax:     5000,
					},
					Checksum:  verification.MakeKVChecksum(4491, 586, 486070148917),
					Timestamp: 1234567894,
				}},
			},
		},
		Checksum: verification.MakeKVChecksum(4492, 686, 486070148910),
	}, cp)
	require.Nil(t, s.mock.ExpectationsWereMet())
}

func TestNormalOperationsWithAddIndexBySQL(t *testing.T) {
	ctx := context.Background()
	s := newCPSQLSuite(t)
	cpdb := s.cpdb

	// 2. initialize with checkpoint data.

	t1Info, err := json.Marshal(&model.TableInfo{
		Name: ast.NewCIStr("t1"),
	})
	require.NoError(t, err)
	t2Info, err := json.Marshal(&model.TableInfo{
		Name: ast.NewCIStr("t2"),
	})
	require.NoError(t, err)
	t3Info, err := json.Marshal(&model.TableInfo{
		Name: ast.NewCIStr("t3"),
	})
	require.NoError(t, err)

	s.mock.ExpectBegin()
	initializeStmt := s.mock.ExpectPrepare(
		"REPLACE INTO `mock-schema`\\.`task_v\\d+`")
	initializeStmt.ExpectExec().
		WithArgs(123, "/data", "local", "127.0.0.1:8287", "127.0.0.1", 4000, "127.0.0.1:2379", "/tmp/sorted-kv", build.ReleaseVersion).
		WillReturnResult(sqlmock.NewResult(6, 1))
	initializeStmt = s.mock.
		ExpectPrepare("INSERT INTO `mock-schema`\\.`table_v\\d+`")
	initializeStmt.ExpectExec().
		WithArgs(123, "`db1`.`t1`", sqlmock.AnyArg(), int64(1), t1Info).
		WillReturnResult(sqlmock.NewResult(7, 1))
	initializeStmt.ExpectExec().
		WithArgs(123, "`db1`.`t2`", sqlmock.AnyArg(), int64(2), t2Info).
		WillReturnResult(sqlmock.NewResult(8, 1))
	initializeStmt.ExpectExec().
		WithArgs(123, "`db2`.`t3`", sqlmock.AnyArg(), int64(3), t3Info).
		WillReturnResult(sqlmock.NewResult(9, 1))
	s.mock.ExpectCommit()

	s.mock.MatchExpectationsInOrder(false)
	cfg := newTestConfig()
	cfg.TikvImporter.AddIndexBySQL = true
	err = cpdb.Initialize(ctx, cfg, map[string]*checkpoints.TidbDBInfo{
		"db1": {
			Name: "db1",
			Tables: map[string]*checkpoints.TidbTableInfo{
				"t1": {
					Name: "t1",
					ID:   1,
					Desired: &model.TableInfo{
						Name: ast.NewCIStr("t1"),
					},
				},
				"t2": {
					Name: "t2",
					ID:   2,
					Desired: &model.TableInfo{
						Name: ast.NewCIStr("t2"),
					},
				},
			},
		},
		"db2": {
			Name: "db2",
			Tables: map[string]*checkpoints.TidbTableInfo{
				"t3": {
					Name: "t3",
					ID:   3,
					Desired: &model.TableInfo{
						Name: ast.NewCIStr("t3"),
					},
				},
			},
		},
	})
	s.mock.MatchExpectationsInOrder(true)
	require.NoError(t, err)
	require.Nil(t, s.mock.ExpectationsWereMet())

	// 3. set some checkpoints

	s.mock.ExpectBegin()
	insertEngineStmt := s.mock.
		ExpectPrepare("REPLACE INTO `mock-schema`\\.`engine_v\\d+` .+")
	insertEngineStmt.
		ExpectExec().
		WithArgs("`db1`.`t2`", 0, 30).
		WillReturnResult(sqlmock.NewResult(8, 1))
	insertEngineStmt.
		ExpectExec().
		WithArgs("`db1`.`t2`", -1, 30).
		WillReturnResult(sqlmock.NewResult(9, 1))
	insertChunkStmt := s.mock.
		ExpectPrepare("REPLACE INTO `mock-schema`\\.`chunk_v\\d+` .+")
	insertChunkStmt.
		ExpectExec().
		WithArgs("`db1`.`t2`", 0, "/tmp/path/1.sql", 0, mydump.SourceTypeSQL, 0, "", 123, []byte("null"), 12, 10, 102400, 1, 5000, 1234567890).
		WillReturnResult(sqlmock.NewResult(10, 1))
	s.mock.ExpectCommit()

	s.mock.MatchExpectationsInOrder(false)
	err = cpdb.InsertEngineCheckpoints(ctx, "`db1`.`t2`", map[int32]*checkpoints.EngineCheckpoint{
		0: {
			Status: checkpoints.CheckpointStatusLoaded,
			Chunks: []*checkpoints.ChunkCheckpoint{{
				Key: checkpoints.ChunkCheckpointKey{
					Path:   "/tmp/path/1.sql",
					Offset: 0,
				},
				FileMeta: mydump.SourceFileMeta{
					Path:     "/tmp/path/1.sql",
					Type:     mydump.SourceTypeSQL,
					FileSize: 123,
				},
				Chunk: mydump.Chunk{
					Offset:       12,
					RealOffset:   10,
					EndOffset:    102400,
					PrevRowIDMax: 1,
					RowIDMax:     5000,
				},
				Timestamp: 1234567890,
			}},
		},
		-1: {
			Status: checkpoints.CheckpointStatusLoaded,
			Chunks: nil,
		},
	})
	s.mock.MatchExpectationsInOrder(true)
	require.NoError(t, err)
	require.Nil(t, s.mock.ExpectationsWereMet())

	// 4. update some checkpoints

	cpd := checkpoints.NewTableCheckpointDiff()
	scm := checkpoints.StatusCheckpointMerger{
		EngineID: 0,
		Status:   checkpoints.CheckpointStatusImported,
	}
	scm.MergeInto(cpd)
	scm = checkpoints.StatusCheckpointMerger{
		EngineID: checkpoints.WholeTableEngineID,
		Status:   checkpoints.CheckpointStatusAllWritten,
	}
	scm.MergeInto(cpd)
	rcm := checkpoints.RebaseCheckpointMerger{
		AutoRandBase:  132861,
		AutoIncrBase:  132862,
		AutoRowIDBase: 132863,
	}
	rcm.MergeInto(cpd)
	cksum := checkpoints.TableChecksumMerger{
		Checksum: verification.MakeKVChecksum(4492, 686, 486070148910),
	}
	cksum.MergeInto(cpd)
	ccm := checkpoints.ChunkCheckpointMerger{
		EngineID: 0,
		Key:      checkpoints.ChunkCheckpointKey{Path: "/tmp/path/1.sql", Offset: 0},
		Checksum: verification.MakeKVChecksum(4491, 586, 486070148917),
		Pos:      55904,
		RealPos:  55902,
		RowID:    681,
	}
	ccm.MergeInto(cpd)

	s.mock.ExpectBegin()
	s.mock.
		ExpectPrepare("UPDATE `mock-schema`\\.`chunk_v\\d+` SET pos = .+").
		ExpectExec().
		WithArgs(
			55904, 55902, 681, 4491, 586, 486070148917, []byte("null"),
			"`db1`.`t2`", 0, "/tmp/path/1.sql", 0,
		).
		WillReturnResult(sqlmock.NewResult(11, 1))
	s.mock.
		ExpectPrepare("UPDATE `mock-schema`\\.`table_v\\d+` SET auto_rand_base = .+ auto_incr_base = .+ auto_row_id_base = .+").
		ExpectExec().
		WithArgs(132861, 132862, 132863, "`db1`.`t2`").
		WillReturnResult(sqlmock.NewResult(12, 1))
	s.mock.
		ExpectPrepare("UPDATE `mock-schema`\\.`engine_v\\d+` SET status = .+").
		ExpectExec().
		WithArgs(120, "`db1`.`t2`", 0).
		WillReturnResult(sqlmock.NewResult(13, 1))
	s.mock.
		ExpectPrepare("UPDATE `mock-schema`\\.`table_v\\d+` SET status = .+").
		ExpectExec().
		WithArgs(60, "`db1`.`t2`").
		WillReturnResult(sqlmock.NewResult(14, 1))
	s.mock.
		ExpectPrepare("UPDATE `mock-schema`\\.`table_v\\d+` SET kv_bytes = .+").
		ExpectExec().
		WithArgs(4492, 686, 486070148910, "`db1`.`t2`").
		WillReturnResult(sqlmock.NewResult(15, 1))

	s.mock.ExpectCommit()

	s.mock.MatchExpectationsInOrder(false)
	cpdb.Update(ctx, map[string]*checkpoints.TableCheckpointDiff{"`db1`.`t2`": cpd})
	s.mock.MatchExpectationsInOrder(true)
	require.Nil(t, s.mock.ExpectationsWereMet())

	// 5. get back the checkpoints

	s.mock.ExpectBegin()
	s.mock.
		ExpectQuery("SELECT .+ FROM `mock-schema`\\.`engine_v\\d+`").
		WithArgs("`db1`.`t2`").
		WillReturnRows(
			sqlmock.NewRows([]string{"engine_id", "status"}).
				AddRow(0, 120).
				AddRow(-1, 30),
		)
	s.mock.
		ExpectQuery("SELECT (?s:.+) FROM `mock-schema`\\.`chunk_v\\d+`").
		WithArgs("`db1`.`t2`").
		WillReturnRows(
			sqlmock.NewRows([]string{
				"engine_id", "path", "offset", "type", "compression", "sort_key", "file_size", "columns",
				"pos", "real_pos", "end_offset", "prev_rowid_max", "rowid_max",
				"kvc_bytes", "kvc_kvs", "kvc_checksum", "unix_timestamp(create_time)",
			}).
				AddRow(
					0, "/tmp/path/1.sql", 0, mydump.SourceTypeSQL, 0, "", 123, "[]",
					55904, 55902, 102400, 681, 5000,
					4491, 586, 486070148917, 1234567894,
				),
		)
	s.mock.
		ExpectQuery("SELECT .+ FROM `mock-schema`\\.`table_v\\d+`").
		WithArgs("`db1`.`t2`").
		WillReturnRows(
			sqlmock.NewRows([]string{
				"status", "table_id", "table_info", "kv_bytes", "kv_kvs", "kv_checksum",
				"auto_rand_base", "auto_incr_base", "auto_row_id_base"}).
				AddRow(60, int64(2), t2Info, uint64(4492), uint64(686), uint64(486070148910), 132861, 132862, 132863),
		)
	s.mock.ExpectCommit()

	cp, err := cpdb.Get(ctx, "`db1`.`t2`")
	require.Nil(t, err)
	require.Equal(t, &checkpoints.TableCheckpoint{
		Status:        checkpoints.CheckpointStatusAllWritten,
		AutoRandBase:  132861,
		AutoIncrBase:  132862,
		AutoRowIDBase: 132863,
		TableID:       int64(2),
		TableInfo: &model.TableInfo{
			Name: ast.NewCIStr("t2"),
		},
		Engines: map[int32]*checkpoints.EngineCheckpoint{
			-1: {Status: checkpoints.CheckpointStatusLoaded},
			0: {
				Status: checkpoints.CheckpointStatusImported,
				Chunks: []*checkpoints.ChunkCheckpoint{{
					Key: checkpoints.ChunkCheckpointKey{
						Path:   "/tmp/path/1.sql",
						Offset: 0,
					},
					FileMeta: mydump.SourceFileMeta{
						Path:     "/tmp/path/1.sql",
						Type:     mydump.SourceTypeSQL,
						FileSize: 123,
					},
					ColumnPermutation: []int{},
					Chunk: mydump.Chunk{
						Offset:       55904,
						RealOffset:   55902,
						EndOffset:    102400,
						PrevRowIDMax: 681,
						RowIDMax:     5000,
					},
					Checksum:  verification.MakeKVChecksum(4491, 586, 486070148917),
					Timestamp: 1234567894,
				}},
			},
		},
		Checksum: verification.MakeKVChecksum(4492, 686, 486070148910),
	}, cp)
	require.Nil(t, s.mock.ExpectationsWereMet())
}

func TestRemoveAllCheckpoints_SQL(t *testing.T) {
	s := newCPSQLSuite(t)

	s.mock.ExpectExec("DROP SCHEMA `mock-schema`").WillReturnResult(sqlmock.NewResult(0, 1))

	ctx := context.Background()

	err := s.cpdb.RemoveCheckpoint(ctx, "all")
	require.NoError(t, err)

	s.mock.ExpectBegin()
	s.mock.
		ExpectQuery("SELECT .+ FROM `mock-schema`\\.`engine_v\\d+`").
		WithArgs("`db1`.`t2`").
		WillReturnRows(sqlmock.NewRows([]string{"engine_id", "status"}))
	s.mock.
		ExpectQuery("SELECT (?s:.+) FROM `mock-schema`\\.`chunk_v\\d+`").
		WithArgs("`db1`.`t2`").
		WillReturnRows(
			sqlmock.NewRows([]string{
				"engine_id", "path", "offset", "type", "compression", "sort_key", "file_size", "columns",
				"pos", "real_pos", "end_offset", "prev_rowid_max", "rowid_max",
				"kvc_bytes", "kvc_kvs", "kvc_checksum", "unix_timestamp(create_time)",
			}))
	s.mock.
		ExpectQuery("SELECT .+ FROM `mock-schema`\\.`table_v\\d+`").
		WithArgs("`db1`.`t2`").
		WillReturnRows(sqlmock.NewRows([]string{"status", "table_id"}))
	s.mock.ExpectRollback()

	cp, err := s.cpdb.Get(ctx, "`db1`.`t2`")
	require.Nil(t, cp)
	require.True(t, errors.IsNotFound(err))
}

func TestRemoveOneCheckpoint_SQL(t *testing.T) {
	s := newCPSQLSuite(t)

	s.mock.ExpectBegin()
	s.mock.
		ExpectExec("DELETE FROM `mock-schema`\\.`chunk_v\\d+` WHERE table_name = \\?").
		WithArgs("`db1`.`t2`").
		WillReturnResult(sqlmock.NewResult(0, 4))
	s.mock.
		ExpectExec("DELETE FROM `mock-schema`\\.`engine_v\\d+` WHERE table_name = \\?").
		WithArgs("`db1`.`t2`").
		WillReturnResult(sqlmock.NewResult(0, 2))
	s.mock.
		ExpectExec("DELETE FROM `mock-schema`\\.`table_v\\d+` WHERE table_name = \\?").
		WithArgs("`db1`.`t2`").
		WillReturnResult(sqlmock.NewResult(0, 1))
	s.mock.ExpectCommit()

	err := s.cpdb.RemoveCheckpoint(context.Background(), "`db1`.`t2`")
	require.NoError(t, err)
}

func TestIgnoreAllErrorCheckpoints_SQL(t *testing.T) {
	s := newCPSQLSuite(t)

	s.mock.ExpectBegin()
	s.mock.
		ExpectExec("UPDATE `mock-schema`\\.`engine_v\\d+` SET status = \\? WHERE status <= \\?").
		WithArgs(checkpoints.CheckpointStatusLoaded, 25).
		WillReturnResult(sqlmock.NewResult(5, 3))
	s.mock.
		ExpectExec("UPDATE `mock-schema`\\.`table_v\\d+` SET status = \\? WHERE status <= \\?").
		WithArgs(checkpoints.CheckpointStatusLoaded, 25).
		WillReturnResult(sqlmock.NewResult(6, 2))
	s.mock.ExpectCommit()

	err := s.cpdb.IgnoreErrorCheckpoint(context.Background(), "all")
	require.NoError(t, err)
}

func TestIgnoreOneErrorCheckpoint(t *testing.T) {
	s := newCPSQLSuite(t)

	s.mock.ExpectBegin()
	s.mock.
		ExpectExec("UPDATE `mock-schema`\\.`engine_v\\d+` SET status = \\? WHERE table_name = \\? AND status <= \\?").
		WithArgs(checkpoints.CheckpointStatusLoaded, "`db1`.`t2`", 25).
		WillReturnResult(sqlmock.NewResult(5, 2))
	s.mock.
		ExpectExec("UPDATE `mock-schema`\\.`table_v\\d+` SET status = \\? WHERE table_name = \\? AND status <= \\?").
		WithArgs(checkpoints.CheckpointStatusLoaded, "`db1`.`t2`", 25).
		WillReturnResult(sqlmock.NewResult(6, 1))
	s.mock.ExpectCommit()

	err := s.cpdb.IgnoreErrorCheckpoint(context.Background(), "`db1`.`t2`")
	require.NoError(t, err)
}

func TestDestroyAllErrorCheckpoints_SQL(t *testing.T) {
	s := newCPSQLSuite(t)

	s.mock.ExpectBegin()
	s.mock.
		ExpectQuery("SELECT (?s:.+)").
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(
			sqlmock.NewRows([]string{"table_name", "__min__", "__max__"}).
				AddRow("`db1`.`t2`", -1, 0),
		)
	s.mock.
		ExpectExec("DELETE FROM `mock-schema`\\.`chunk_v\\d+` WHERE table_name IN").
		WithArgs(sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 5))
	s.mock.
		ExpectExec("DELETE FROM `mock-schema`\\.`engine_v\\d+` WHERE table_name IN").
		WithArgs(sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 3))
	s.mock.
		ExpectExec("DELETE FROM `mock-schema`\\.`table_v\\d+`").
		WithArgs(sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 2))
	s.mock.ExpectCommit()

	dtc, err := s.cpdb.DestroyErrorCheckpoint(context.Background(), "all")
	require.NoError(t, err)
	require.Equal(t, []checkpoints.DestroyedTableCheckpoint{{
		TableName:   "`db1`.`t2`",
		MinEngineID: -1,
		MaxEngineID: 0,
	}}, dtc)
}

func TestDestroyOneErrorCheckpoints(t *testing.T) {
	s := newCPSQLSuite(t)

	s.mock.ExpectBegin()
	s.mock.
		ExpectQuery("SELECT (?s:.+)table_name = \\?").
		WithArgs("`db1`.`t2`", sqlmock.AnyArg()).
		WillReturnRows(
			sqlmock.NewRows([]string{"table_name", "__min__", "__max__"}).
				AddRow("`db1`.`t2`", -1, 0),
		)
	s.mock.
		ExpectExec("DELETE FROM `mock-schema`\\.`chunk_v\\d+` WHERE .+table_name = \\?").
		WithArgs("`db1`.`t2`", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 4))
	s.mock.
		ExpectExec("DELETE FROM `mock-schema`\\.`engine_v\\d+` WHERE .+table_name = \\?").
		WithArgs("`db1`.`t2`", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 2))
	s.mock.
		ExpectExec("DELETE FROM `mock-schema`\\.`table_v\\d+` WHERE table_name = \\?").
		WithArgs("`db1`.`t2`", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	s.mock.ExpectCommit()

	dtc, err := s.cpdb.DestroyErrorCheckpoint(context.Background(), "`db1`.`t2`")
	require.NoError(t, err)
	require.Equal(t, []checkpoints.DestroyedTableCheckpoint{{
		TableName:   "`db1`.`t2`",
		MinEngineID: -1,
		MaxEngineID: 0,
	}}, dtc)
}

func TestDump(t *testing.T) {
	ctx := context.Background()
	s := newCPSQLSuite(t)
	tm := time.Unix(1555555555, 0).UTC()

	s.mock.
		ExpectQuery("SELECT (?s:.+) FROM `mock-schema`\\.`chunk_v\\d+`").
		WillReturnRows(
			sqlmock.NewRows([]string{
				"table_name", "path", "offset", "type", "compression", "sort_key", "file_size", "columns",
				"pos", "real_pos", "end_offset", "prev_rowid_max", "rowid_max",
				"kvc_bytes", "kvc_kvs", "kvc_checksum",
				"create_time", "update_time",
			}).AddRow(
				"`db1`.`t2`", "/tmp/path/1.sql", 0, mydump.SourceTypeSQL, mydump.CompressionNone, "", 456, "[]",
				55904, 55902, 102400, 681, 5000,
				4491, 586, 486070148917,
				tm, tm,
			),
		)

	var csvBuilder strings.Builder
	err := s.cpdb.DumpChunks(ctx, &csvBuilder)
	require.NoError(t, err)
	require.Equal(t,
		"table_name,path,offset,type,compression,sort_key,file_size,columns,pos,real_pos,end_offset,prev_rowid_max,rowid_max,kvc_bytes,kvc_kvs,kvc_checksum,create_time,update_time\n"+
			"`db1`.`t2`,/tmp/path/1.sql,0,3,0,,456,[],55904,55902,102400,681,5000,4491,586,486070148917,2019-04-18 02:45:55 +0000 UTC,2019-04-18 02:45:55 +0000 UTC\n",
		csvBuilder.String(),
	)

	s.mock.
		ExpectQuery("SELECT .+ FROM `mock-schema`\\.`engine_v\\d+`").
		WillReturnRows(
			sqlmock.NewRows([]string{"table_name", "engine_id", "status", "create_time", "update_time"}).
				AddRow("`db1`.`t2`", -1, 30, tm, tm).
				AddRow("`db1`.`t2`", 0, 120, tm, tm),
		)

	csvBuilder.Reset()
	err = s.cpdb.DumpEngines(ctx, &csvBuilder)
	require.NoError(t, err)
	require.Equal(t, "table_name,engine_id,status,create_time,update_time\n"+
		"`db1`.`t2`,-1,30,2019-04-18 02:45:55 +0000 UTC,2019-04-18 02:45:55 +0000 UTC\n"+
		"`db1`.`t2`,0,120,2019-04-18 02:45:55 +0000 UTC,2019-04-18 02:45:55 +0000 UTC\n",
		csvBuilder.String())

	s.mock.
		ExpectQuery("SELECT .+ FROM `mock-schema`\\.`table_v\\d+`").
		WillReturnRows(sqlmock.NewRows([]string{"task_id", "table_name", "hash", "status",
			"create_time", "update_time", "auto_rand_base", "auto_incr_base", "auto_row_id_base"}).
			AddRow(1555555555, "`db1`.`t2`", 0, 90, tm, tm, 132861, 132862, 132863),
		)

	csvBuilder.Reset()
	err = s.cpdb.DumpTables(ctx, &csvBuilder)
	require.NoError(t, err)
	require.Equal(t, "task_id,table_name,hash,status,create_time,update_time,auto_rand_base,auto_incr_base,auto_row_id_base\n"+
		"1555555555,`db1`.`t2`,0,90,2019-04-18 02:45:55 +0000 UTC,2019-04-18 02:45:55 +0000 UTC,132861,132862,132863\n",
		csvBuilder.String(),
	)
}

func TestMoveCheckpoints(t *testing.T) {
	ctx := context.Background()
	s := newCPSQLSuite(t)

	s.mock.
		ExpectExec("CREATE SCHEMA IF NOT EXISTS `mock-schema\\.12345678\\.bak`").
		WillReturnResult(sqlmock.NewResult(1, 1))
	s.mock.
		ExpectExec("RENAME TABLE `mock-schema`\\.`chunk_v\\d+` TO `mock-schema\\.12345678\\.bak`\\.`chunk_v\\d+`").
		WillReturnResult(sqlmock.NewResult(0, 1))
	s.mock.
		ExpectExec("RENAME TABLE `mock-schema`\\.`engine_v\\d+` TO `mock-schema\\.12345678\\.bak`\\.`engine_v\\d+`").
		WillReturnResult(sqlmock.NewResult(0, 1))
	s.mock.
		ExpectExec("RENAME TABLE `mock-schema`\\.`table_v\\d+` TO `mock-schema\\.12345678\\.bak`\\.`table_v\\d+`").
		WillReturnResult(sqlmock.NewResult(0, 1))
	s.mock.
		ExpectExec("RENAME TABLE `mock-schema`\\.`task_v\\d+` TO `mock-schema\\.12345678\\.bak`\\.`task_v\\d+`").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := s.cpdb.MoveCheckpoints(ctx, 12345678)
	require.NoError(t, err)
}
