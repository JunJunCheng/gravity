package mysqlbatch

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/juju/errors"

	"github.com/moiot/gravity/pkg/utils"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/moiot/gravity/pkg/core"
	"github.com/moiot/gravity/pkg/emitter"
	"github.com/moiot/gravity/pkg/mysql_test"
	"github.com/moiot/gravity/pkg/position_store"
	"github.com/moiot/gravity/pkg/schema_store"
)

func TestFindMaxMinValueCompositePks(t *testing.T) {
	r := require.New(t)

	testDBName := utils.TestCaseMd5Name(t)
	db := mysql_test.MustSetupSourceDB(testDBName)
	defer db.Close()

	for i := 1; i < 100; i++ {
		args := map[string]interface{}{
			"id":    i,
			"name":  fmt.Sprintf("name_%d", i),
			"email": fmt.Sprintf("email_%d", i),
		}
		err := mysql_test.InsertIntoTestTable(db, testDBName, mysql_test.TestScanColumnTableCompositePrimaryOutOfOrder, args)
		r.NoError(err)
	}

	max, min := FindMaxMinValueFromDB(db, testDBName, mysql_test.TestScanColumnTableCompositePrimaryOutOfOrder, []string{"email", "name"})

	maxEmail, ok := max[0].(sql.NullString)
	r.True(ok)
	r.Equal("email_99", maxEmail.String)

	maxName, ok := max[1].(sql.NullString)
	r.True(ok)
	r.Equal("name_99", maxName.String)

	minEmail, ok := min[0].(sql.NullString)
	r.True(ok)
	r.Equal("email_1", minEmail.String)

	minName, ok := min[1].(sql.NullString)
	r.True(ok)
	r.Equal("name_1", minName.String)
}

func TestFindMaxMinValueInt(t *testing.T) {
	r := require.New(t)
	testDBName := "mysql_table_scanner_test_1"

	db := mysql_test.MustSetupSourceDB(testDBName)
	defer db.Close()

	for i := 1; i < 100; i++ {
		args := map[string]interface{}{
			"id": i,
		}
		err := mysql_test.InsertIntoTestTable(db, testDBName, mysql_test.TestTableName, args)
		r.NoError(err)
	}

	max, min := FindMaxMinValueFromDB(db, testDBName, mysql_test.TestTableName, []string{"id"})

	maxVal, ok := max[0].(uint32)

	r.EqualValues(99, maxVal)

	minVal, ok := min[0].(uint32)
	r.True(ok)

	r.EqualValues(1, minVal)
}

func TestFindMaxMinValueString(t *testing.T) {
	r := require.New(t)
	testDBName := "mysql_table_scanner_test_2"

	db := mysql_test.MustSetupSourceDB(testDBName)
	defer db.Close()

	for i := 1; i <= 2; i++ {
		name := fmt.Sprintf("test_%d", i)
		args := map[string]interface{}{
			"id":   i,
			"name": name,
		}
		err := mysql_test.InsertIntoTestTable(db, testDBName, mysql_test.TestTableName, args)
		if err != nil {
			r.FailNow(err.Error())
		}
	}

	count, err := mysql_test.CountTestTable(db, testDBName, mysql_test.TestTableName)
	r.NoError(err)
	r.EqualValues(2, count)

	max, min := FindMaxMinValueFromDB(db, testDBName, mysql_test.TestTableName, []string{"name"})

	maxV, ok1 := max[0].(sql.NullString)
	r.True(ok1)

	minV, ok2 := min[0].(sql.NullString)
	r.True(ok2)
	r.Equal("test_2", maxV.String)
	r.Equal("test_1", minV.String)
}

func TestFindMaxMinValueTime(t *testing.T) {
	r := require.New(t)
	testDBName := "mysql_table_scanner_test_3"

	db := mysql_test.MustSetupSourceDB(testDBName)
	startTime := time.Now()
	for i := 0; i < 100; i++ {
		args := map[string]interface{}{
			"id": i,
			"ts": startTime.Add(time.Duration(i) * time.Minute),
		}
		err := mysql_test.InsertIntoTestTable(db, testDBName, mysql_test.TestTableName, args)
		r.Nil(err)
	}

	max, min := FindMaxMinValueFromDB(db, testDBName, mysql_test.TestTableName, []string{"ts"})
	maxT := max[0].(mysql.NullTime)
	minT := min[0].(mysql.NullTime)
	// assert.True(t, reflect.DeepEqual(mysql.NullTime{Time: startTime.Add(99 * time.Second), Valid: true}, maxT))
	// assert.True(t, reflect.DeepEqual(mysql.NullTime{Time: startTime, Valid: true}, minT))
	assert.EqualValues(t, startTime.Add(99*time.Minute).Minute(), maxT.Time.Minute())
	assert.EqualValues(t, startTime.Minute(), minT.Time.Minute())
}

type fakeMsgSubmitter struct {
	msgs []*core.Msg
}

func (submitter *fakeMsgSubmitter) SubmitMsg(msg *core.Msg) error {
	if msg.Type == core.MsgDML {
		submitter.msgs = append(submitter.msgs, msg)
	}
	if msg.AfterCommitCallback != nil {
		if err := msg.AfterCommitCallback(msg); err != nil {
			return errors.Trace(err)
		}
	}
	close(msg.Done)
	return nil
}

func TestTableScanner_Start(t *testing.T) {
	r := require.New(t)

	t.Run("it terminates", func(tt *testing.T) {
		testDBName := utils.TestCaseMd5Name(tt)

		dbCfg := mysql_test.SourceDBConfig()
		positionRepo, err := position_store.NewMySQLRepo(dbCfg, "")
		r.NoError(err)

		testCases := []struct {
			name        string
			seedFunc    func(db *sql.DB)
			cfg         PluginConfig
			scanColumns []string
		}{
			{
				"no record in table",
				nil,
				PluginConfig{
					Source: dbCfg,
					TableConfigs: []TableConfig{
						{
							Schema: testDBName,
							Table:  []string{mysql_test.TestScanColumnTableIdPrimary},
						},
					},
					NrScanner:           1,
					TableScanBatch:      1,
					BatchPerSecondLimit: 10000,
				},
				[]string{"id"},
			},
			{
				"sends one msg when source table have only one record",
				func(db *sql.DB) {
					args := map[string]interface{}{
						"id":   1,
						"name": "name",
					}
					mysql_test.InsertIntoTestTable(db, testDBName, mysql_test.TestScanColumnTableIdPrimary, args)
				},
				PluginConfig{
					Source: dbCfg,
					TableConfigs: []TableConfig{
						{
							Schema: testDBName,
							Table:  []string{mysql_test.TestScanColumnTableIdPrimary},
						},
					},
					NrScanner:           1,
					TableScanBatch:      1,
					BatchPerSecondLimit: 10000,
				},
				[]string{"id"},
			},
			{
				"terminates when scan column is int",
				func(db *sql.DB) {
					for i := 1; i < 10; i++ {
						args := map[string]interface{}{
							"id": i,
						}
						r.NoError(mysql_test.InsertIntoTestTable(db, testDBName, mysql_test.TestScanColumnTableIdPrimary, args))
					}
				},
				PluginConfig{
					Source: dbCfg,
					TableConfigs: []TableConfig{
						{
							Schema: testDBName,
							Table:  []string{mysql_test.TestScanColumnTableIdPrimary},
						},
					},
					NrScanner:           1,
					TableScanBatch:      1,
					BatchPerSecondLimit: 10000,
				},
				[]string{"id"},
			},
			{
				"terminates when scan column is string",
				func(db *sql.DB) {
					t := time.Now()
					for i := 1; i < 10; i++ {
						t.Add(time.Second)
						args := map[string]interface{}{
							"id":    i,
							"email": fmt.Sprintf("email_%d", i),
							"ts":    t,
						}
						err := mysql_test.InsertIntoTestTable(db, testDBName, mysql_test.TestScanColumnTableUniqueIndexEmailString, args)
						r.NoError(err)

					}
				},
				PluginConfig{
					Source: dbCfg,
					TableConfigs: []TableConfig{
						{
							Schema: testDBName,
							Table:  []string{mysql_test.TestScanColumnTableUniqueIndexEmailString},
						},
					},
					NrScanner:           1,
					TableScanBatch:      1,
					BatchPerSecondLimit: 10000,
				},
				[]string{"email"},
			},
			{
				"terminates when scan column is time",
				func(db *sql.DB) {
					t := time.Now()
					for i := 1; i < 10; i++ {
						t = t.Add(1000 * time.Second)
						args := map[string]interface{}{
							"id": i,
							"ts": t,
						}
						r.NoError(mysql_test.InsertIntoTestTable(db, testDBName, mysql_test.TestScanColumnTableUniqueIndexTime, args))
					}
				},
				PluginConfig{
					Source: dbCfg,
					TableConfigs: []TableConfig{
						{
							Schema: testDBName,
							Table:  []string{mysql_test.TestScanColumnTableUniqueIndexTime},
						},
					},
					NrScanner:           1,
					TableScanBatch:      1,
					BatchPerSecondLimit: 10000,
				},
				[]string{"ts"},
			},
			{
				"terminates when do a full scan",
				func(db *sql.DB) {
					for i := 1; i < 10; i++ {
						args := map[string]interface{}{
							"id": i,
						}
						err := mysql_test.InsertIntoTestTable(db, testDBName, mysql_test.TestScanColumnTableNoKey, args)
						r.NoError(err)
					}

				},
				PluginConfig{
					Source: dbCfg,
					TableConfigs: []TableConfig{
						{
							Schema: testDBName,
							Table:  []string{mysql_test.TestScanColumnTableNoKey},
						},
					},
					NrScanner:           1,
					TableScanBatch:      1,
					BatchPerSecondLimit: 10000,
				},
				[]string{ScanColumnForDump},
			},
			{
				"terminates when table has composite primary key",
				func(db *sql.DB) {
					for i := 1; i < 10; i++ {
						args := map[string]interface{}{
							"id":    i,
							"name":  fmt.Sprintf("name_%d", i),
							"email": fmt.Sprintf("email_%d", i),
						}
						err := mysql_test.InsertIntoTestTable(db, testDBName, mysql_test.TestScanColumnTableCompositePrimaryOutOfOrder, args)
						r.NoError(err)
					}

				},
				PluginConfig{
					Source: dbCfg,
					TableConfigs: []TableConfig{
						{
							Schema: testDBName,
							Table:  []string{mysql_test.TestScanColumnTableCompositePrimaryOutOfOrder},
						},
					},
					NrScanner:           1,
					TableScanBatch:      1,
					BatchPerSecondLimit: 10000,
				},
				[]string{ScanColumnForDump},
			},
			// {
			// 	"terminates when table has composite unique key",
			// 	func(db *sql.DB) {
			// 		for i := 1; i < 10; i++ {
			// 			args := map[string]interface{}{
			// 				"id": i,
			// 				"ts": time.Now(),
			// 			}
			// 			err := mysql_test.InsertIntoTestTable(db, testDBName, mysql_test.TestScanColumnTableCompositeUniqueKey, args)
			// 			r.NoError(err)
			// 		}
			//
			// 	},
			// 	PluginConfig{
			// 		Source: dbCfg,
			// 		TableConfigs: []TableConfig{
			// 			{
			// 				Schema: testDBName,
			// 				Table:  []string{mysql_test.TestScanColumnTableCompositeUniqueKey},
			// 			},
			// 		},
			// 		NrScanner:           1,
			// 		TableScanBatch:      1,
			// 		BatchPerSecondLimit: 10000,
			// 	},
			// 	[]string{ScanColumnForDump},
			//
			// },
		}

		for _, c := range testCases {
			err := c.cfg.ValidateAndSetDefault()
			r.NoError(err)

			db := mysql_test.MustSetupSourceDB(testDBName)

			if c.seedFunc != nil {
				c.seedFunc(db)
			}

			schemaStore, err := schema_store.NewSimpleSchemaStoreFromDBConn(db)
			r.NoError(err)
			cfg := c.cfg

			tableDefs, tableConfigs := GetTables(db, schemaStore, cfg.TableConfigs, nil)
			r.Equal(1, len(tableDefs))
			r.Equal(1, len(tableConfigs))

			throttle := time.NewTicker(100 * time.Millisecond)

			positionCache, err := position_store.NewPositionCache(
				testDBName,
				positionRepo,
				EncodeBatchPositionValue,
				DecodeBatchPositionValue,
				10*time.Second)
			r.NoError(err)

			r.NoError(SetupInitialPosition(positionCache, db))

			for i := range tableDefs {
				// empty table should be ignored
				if c.seedFunc == nil {
					tableDefs, tableConfigs = DeleteEmptyTables(db, tableDefs, tableConfigs)
					r.Equal(0, len(tableDefs))
				} else {
					_, err := InitTablePosition(db, positionCache, tableDefs[i], c.scanColumns, 100)
					r.NoError(err)
				}
			}

			if len(tableDefs) == 0 {
				continue
			}

			submitter := &fakeMsgSubmitter{}
			em, err := emitter.NewEmitter(nil, submitter)
			r.NoError(err)

			q := make(chan *TableWork, 1)
			q <- &TableWork{TableDef: tableDefs[0], TableConfig: &tableConfigs[0], ScanColumns: c.scanColumns}
			close(q)

			// randomly and delete the max value
			deleteMaxValueRandomly(
				db,
				utils.TableIdentity(tableDefs[0].Schema, tableDefs[0].Name),
				positionCache,
			)

			tableScanner := NewTableScanner(
				tt.Name(),
				q,
				db,
				positionCache,
				em,
				throttle,
				schemaStore,
				&cfg,
				context.Background(),
			)
			r.NoError(tableScanner.Start())
			tableScanner.Wait()

			// do it again, the submitter should not receive any message now.
			submitter = &fakeMsgSubmitter{}
			em, err = emitter.NewEmitter(nil, submitter)
			r.NoError(err)

			positionCache, err = position_store.NewPositionCache(
				testDBName,
				positionRepo,
				EncodeBatchPositionValue,
				DecodeBatchPositionValue,
				10*time.Second)
			r.NoError(err)

			q = make(chan *TableWork, 1)
			q <- &TableWork{TableDef: tableDefs[0], TableConfig: &tableConfigs[0], ScanColumns: c.scanColumns}
			close(q)

			tableScanner = NewTableScanner(
				tt.Name(),
				q,
				db,
				positionCache,
				em,
				throttle,
				schemaStore,
				&cfg,
				context.Background(),
			)
			r.NoError(tableScanner.Start())
			tableScanner.Wait()
			r.Equalf(0, len(submitter.msgs), "test case: %v", c.name)
		}
	})
}

func TestGenerateScanQueryAndArgs(t *testing.T) {
	r := require.New(t)

	sql, args := GenerateScanQueryAndArgs(
		false,
		"a.b",
		[]string{"v1", "v2", "v3"},
		[]interface{}{"1", 1, 10},
		[]interface{}{"9999", 9999, 10000},
		100)
	r.Equal("SELECT * FROM a.b WHERE v1 > ? AND v1 <= ? AND v2 > ? AND v2 <= ? AND v3 > ? AND v3 <= ? ORDER BY v1, v2, v3 LIMIT ?", sql)
	r.EqualValues("1", args[0])
	r.EqualValues("9999", args[1])
	r.EqualValues(1, args[2])
	r.EqualValues(9999, args[3])
	r.EqualValues(10, args[4])
	r.EqualValues(10000, args[5])
	r.EqualValues(100, args[6])
}

func deleteMaxValueRandomly(db *sql.DB, fullTableName string, positionCache position_store.PositionCacheInterface) {
	p, exists, err := positionCache.Get()
	if err != nil {
		panic(err.Error())
	}

	if !exists {
		panic("empty position")
	}

	batchPositionValue := p.Value.(BatchPositionValueV1)
	stats := batchPositionValue.TableStates[fullTableName]
	// Skip multiple scan key and full dump
	if len(stats.Max) != 1 || stats.Max[0].Column == "*" {
		return
	}
	if rand.Float32() < 0.5 {
		_, err := db.Exec(fmt.Sprintf("DELETE FROM %s WHERE %s = ?", fullTableName, stats.Max[0].Column), stats.Max[0].Value)
		if err != nil {
			panic(err.Error())
		}
	}

}
