/*
 * Copyright (c) "Neo4j"
 * Neo4j Sweden AB [http://neo4j.com]
 *
 * This file is part of Neo4j.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
 */

package test_integration

import (
	"crypto/rand"
	"math"
	"math/big"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v4/neo4j/db"
	"github.com/neo4j/neo4j-go-driver/v4/neo4j/internal/bolt"
	. "github.com/neo4j/neo4j-go-driver/v4/neo4j/internal/testutil"
	"github.com/neo4j/neo4j-go-driver/v4/neo4j/log"
	"github.com/neo4j/neo4j-go-driver/v4/neo4j/test-integration/dbserver"
)

func makeRawConnection(logger log.Logger, boltLogger log.BoltLogger) (dbserver.DbServer, db.Connection) {
	server := dbserver.GetDbServer()
	uri := server.BoltURI()
	parsedUri, err := url.Parse(uri)
	if err != nil {
		panic(err)
	}

	tcpConn, err := net.Dial("tcp", parsedUri.Host)
	if err != nil {
		panic(err)
	}

	authMap := map[string]interface{}{
		"scheme":      "basic",
		"principal":   server.Username,
		"credentials": server.Password,
	}

	boltConn, err := bolt.Connect(parsedUri.Host, tcpConn, authMap, "007", nil, logger, boltLogger)
	if err != nil {
		panic(err)
	}
	return server, boltConn
}

func BenchmarkQuery(b *testing.B) {
	_, conn := makeRawConnection(&log.Console{Debugs: true, Errors: true, Infos: true, Warns: true}, nil)
	defer conn.Close()
	params := map[string]interface{}{
		"one": 1,
		"arr": []int{1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024, 2048, 4096, 8192, 16384, 32768, 65536},
		"str": "bAbOgocy84hxL0UFyAeScQUJQqunrP5a2dxAI54mF9vm4YUfhT0wgrcUQqLsC2QauCuzWRgliXB07kdRIzLZqATHHqQxwZFkVnpB",
	}

	for i := 0; i < b.N; i++ {
		stream, _ := conn.Run(db.Command{Cypher: "RETURN $one, $arr, $str", Params: params}, db.TxConfig{Mode: db.ReadMode})
		record, _, _ := conn.Next(stream)

		if len(record.Values) != 3 {
			panic("")
		}
	}
}

// Tests the specification of the internal db connection API
func TestConnectionConformance(ot *testing.T) {
	server, boltConn := makeRawConnection(&log.Console{Errors: true, Infos: true, Warns: true, Debugs: true}, nil)
	defer boltConn.Close()

	randInt := func() int64 {
		bid, _ := rand.Int(rand.Reader, big.NewInt(math.MaxInt64))
		return bid.Int64()
	}

	// All of these tests should leave the connection in a good state without the need
	// for a reset. All tests share the same connection.
	cases := []struct {
		name string
		fun  func(*testing.T, db.Connection)
	}{
		{
			// Leaves the connection in perfect state after creating and iterating through all records
			name: "Run autocommit, full consume",
			fun: func(t *testing.T, c db.Connection) {
				s, err := c.Run(db.Command{Cypher: "CREATE (n:Rand {val: $r}) RETURN n", Params: map[string]interface{}{"r": randInt()}}, db.TxConfig{Mode: db.WriteMode})
				AssertNoError(t, err)
				rec, sum, err := c.Next(s)
				AssertNextOnlyRecord(t, rec, sum, err)
				rec, sum, err = c.Next(s)
				AssertNextOnlySummary(t, rec, sum, err)
			},
		},
		{
			// Let the connection buffer the result before next autocommit.
			// Leaves the connection in streaming state from the last Run.
			name: "Run autocommit twice, no consume",
			fun: func(t *testing.T, c db.Connection) {
				_, err := c.Run(db.Command{Cypher: "CREATE (n:Rand {val: $r})", Params: map[string]interface{}{"r": randInt()}}, db.TxConfig{Mode: db.WriteMode})
				AssertNoError(t, err)
				_, err = c.Run(db.Command{Cypher: "CREATE (n:Rand {val: $r})", Params: map[string]interface{}{"r": randInt()}}, db.TxConfig{Mode: db.WriteMode})
				AssertNoError(t, err)
			},
		},
		{
			// Iterate everything before committing
			name: "Run explicit commit, full consume",
			fun: func(t *testing.T, c db.Connection) {
				txHandle, err := c.TxBegin(db.TxConfig{Mode: db.WriteMode, Timeout: 10 * time.Minute})
				AssertNoError(t, err)
				r := randInt()
				s, err := c.RunTx(txHandle, db.Command{Cypher: "CREATE (n:Rand {val: $r}) RETURN n", Params: map[string]interface{}{"r": r}})
				AssertNoError(t, err)
				rec, sum, err := c.Next(s)
				AssertNextOnlyRecord(t, rec, sum, err)
				rec, sum, err = c.Next(s)
				AssertNextOnlySummary(t, rec, sum, err)
				err = c.TxCommit(txHandle)
				AssertNoError(t, err)
				// Make sure it's commited
				s, err = c.Run(db.Command{Cypher: "MATCH (n:Rand {val: $r}) RETURN n", Params: map[string]interface{}{"r": r}}, db.TxConfig{Mode: db.ReadMode})
				AssertNoError(t, err)
				rec, sum, err = c.Next(s)
				AssertNextOnlyRecord(t, rec, sum, err)
				// Not everything consumed from the read check, but that is also fine
			},
		},
		{
			// Do not consume anything before commiting
			name: "Run explicit commit, no consume",
			fun: func(t *testing.T, c db.Connection) {
				txHandle, err := c.TxBegin(db.TxConfig{Mode: db.WriteMode, Timeout: 10 * time.Minute})
				AssertNoError(t, err)
				r := randInt()
				s, err := c.RunTx(txHandle, db.Command{Cypher: "CREATE (n:Rand {val: $r}) RETURN n", Params: map[string]interface{}{"r": r}})
				AssertNoError(t, err)
				err = c.TxCommit(txHandle)
				AssertNoError(t, err)
				// Make sure it's commited
				s, err = c.Run(db.Command{Cypher: "MATCH (n:Rand {val: $r}) RETURN n", Params: map[string]interface{}{"r": r}}, db.TxConfig{Mode: db.ReadMode})
				AssertNoError(t, err)
				rec, sum, err := c.Next(s)
				AssertNextOnlyRecord(t, rec, sum, err)
				// Not everything consumed from the read check, but that is also fine
			},
		},
		{
			// Iterate everything returned from create before rolling back
			name: "Run explicit rollback, full consume",
			fun: func(t *testing.T, c db.Connection) {
				txHandle, err := c.TxBegin(db.TxConfig{Mode: db.WriteMode, Timeout: 10 * time.Minute})
				AssertNoError(t, err)
				r := randInt()
				s, err := c.RunTx(txHandle, db.Command{Cypher: "CREATE (n:Rand {val: $r}) RETURN n", Params: map[string]interface{}{"r": r}})
				AssertNoError(t, err)
				rec, sum, err := c.Next(s)
				AssertNextOnlyRecord(t, rec, sum, err)
				rec, sum, err = c.Next(s)
				AssertNextOnlySummary(t, rec, sum, err)
				err = c.TxRollback(txHandle)
				AssertNoError(t, err)
				// Make sure it's rolled back
				s, err = c.Run(db.Command{Cypher: "MATCH (n:Rand {val: $r}) RETURN n", Params: map[string]interface{}{"r": r}}, db.TxConfig{Mode: db.ReadMode})
				AssertNoError(t, err)
				rec, sum, err = c.Next(s)
				AssertNextOnlySummary(t, rec, sum, err)
			},
		},
		{
			// Do not consume anything before rolling back
			name: "Run explicit rollback, no consume",
			fun: func(t *testing.T, c db.Connection) {
				txHandle, err := c.TxBegin(db.TxConfig{Mode: db.WriteMode, Timeout: 10 * time.Minute})
				AssertNoError(t, err)
				r := randInt()
				s, err := c.RunTx(txHandle, db.Command{Cypher: "CREATE (n:Rand {val: $r}) RETURN n", Params: map[string]interface{}{"r": r}})
				AssertNoError(t, err)
				err = c.TxRollback(txHandle)
				AssertNoError(t, err)
				// Make sure it's commited
				s, err = c.Run(db.Command{Cypher: "MATCH (n:Rand {val: $r}) RETURN n", Params: map[string]interface{}{"r": r}}, db.TxConfig{Mode: db.ReadMode})
				AssertNoError(t, err)
				rec, sum, err := c.Next(s)
				AssertNextOnlySummary(t, rec, sum, err)
			},
		},
		{
			name: "Nested results in transaction, iterate outer result",
			fun: func(t *testing.T, c db.Connection) {
				tx, err := c.TxBegin(db.TxConfig{Mode: db.ReadMode})
				AssertNoError(t, err)
				r1, err := c.RunTx(tx, db.Command{Cypher: "UNWIND RANGE(0, 100) AS n RETURN n"})
				AssertNoError(t, err)
				n := int64(0)
				rec, _, _ := c.Next(r1)
				for ; rec != nil; rec, _, _ = c.Next(r1) {
					n = rec.Values[0].(int64)
					_, err := c.RunTx(tx, db.Command{Cypher: "UNWIND RANGE (0, $x) AS x RETURN x", Params: map[string]interface{}{"x": n}})
					AssertNoError(t, err)
				}
				if n != 100 {
					t.Errorf("n should have reached 100: %d", n)
				}
				err = c.TxCommit(tx)
				AssertNoError(t, err)
				AssertStringNotEmpty(t, c.Bookmark())
			},
		},
		{
			name: "Next without streaming",
			fun: func(t *testing.T, c db.Connection) {
				rec, sum, err := c.Next(3)
				AssertNextOnlyError(t, rec, sum, err)
			},
		},
		{
			name: "Next passed the summary",
			fun: func(t *testing.T, c db.Connection) {
				s, err := boltConn.Run(db.Command{Cypher: "RETURN 42"}, db.TxConfig{Mode: db.ReadMode})
				AssertNoError(t, err)
				rec, sum, err := c.Next(s)
				AssertNextOnlyRecord(t, rec, sum, err)
				rec, sum, err = c.Next(s)
				AssertNextOnlySummary(t, rec, sum, err)
				rec, sum, err = c.Next(s)
				AssertNextOnlySummary(t, rec, sum, err)
			},
		},
		{
			name: "Run autocommit while in tx",
			fun: func(t *testing.T, c db.Connection) {
				txHandle, err := c.TxBegin(db.TxConfig{Mode: db.WriteMode, Timeout: 10 * time.Minute})
				AssertNoError(t, err)
				defer c.TxRollback(txHandle)
				s, err := c.Run(db.Command{Cypher: "CREATE (n:Rand {val: $r})", Params: map[string]interface{}{"r": randInt()}}, db.TxConfig{Mode: db.WriteMode})
				if s != nil || err == nil {
					t.Fatal("Should fail to run auto commit when in transaction")
				}
				// TODO: Assert type of error!
			},
		},
		{
			name: "Commit while not in tx",
			fun: func(t *testing.T, c db.Connection) {
				err := c.TxCommit(1)
				if err == nil {
					t.Fatal("Should have failed")
				}
				// TODO: Assert type of error!
			},
		},
		{
			name: "Rollback while not in tx",
			fun: func(t *testing.T, c db.Connection) {
				err := c.TxRollback(3)
				if err == nil {
					t.Fatal("Should have failed")
				}
				// TODO: Assert type of error!
			},
		},
	}
	// Run all above in sequence
	for _, c := range cases {
		ot.Run(c.name, func(t *testing.T) {
			c.fun(t, boltConn)
			if !boltConn.IsAlive() {
				t.Error("Connection died")
			}
		})
	}
	// Run some of the above at random as one test
	ot.Run("Random sequence", func(t *testing.T) {
		randoms := make([]int, 25)
		for i := range randoms {
			randoms[i] = int(randInt() % int64(len(cases)))
		}
		for _, i := range randoms {
			c := cases[i]
			c.fun(t, boltConn)
			if !boltConn.IsAlive() {
				t.Error("Connection died")
			}
		}
	})

	// All of these tests should leave the connection in a good state after a reset but not
	// necessarily without it. All tests share the same connection.
	cases = []struct {
		name string
		fun  func(*testing.T, db.Connection)
	}{
		// Connection is in failed state due to syntax error
		{
			name: "Run autocommit with syntax error",
			fun: func(t *testing.T, c db.Connection) {
				s, err := c.Run(db.Command{Cypher: "MATCH (n:Rand {val: $r} ", Params: map[string]interface{}{"r": randInt()}}, db.TxConfig{Mode: db.ReadMode})
				if err == nil || s != nil {
					t.Fatal("Should have received error")
				}
				_, isDbError := err.(*db.Neo4jError)
				if !isDbError {
					t.Error("Should be db error")
				}
			},
		},
		{
			name: "Run autocommit with division by zero in result",
			fun: func(t *testing.T, c db.Connection) {
				s, err := c.Run(db.Command{Cypher: "UNWIND [0] AS x RETURN 10 / x", Params: map[string]interface{}{"r": randInt()}}, db.TxConfig{Mode: db.ReadMode})
				AssertNoError(t, err)
				// Should get error while iterating
				_, _, err = c.Next(s)
				if err == nil {
					t.Error("Should have error")
				}
				_, isDbError := err.(*db.Neo4jError)
				if !isDbError {
					t.Error("Should be db error")
				}
			},
		},
		// Connection is in transaction (lazy)
		{
			name: "Set connection in transaction mode",
			fun: func(t *testing.T, c db.Connection) {
				_, err := c.TxBegin(db.TxConfig{Mode: db.WriteMode})
				AssertNoError(t, err)
			},
		},
		// Connection is in transaction
		{
			name: "Set connection in transaction mode",
			fun: func(t *testing.T, c db.Connection) {
				tx, err := c.TxBegin(db.TxConfig{Mode: db.WriteMode})
				_, err = c.RunTx(tx, db.Command{Cypher: "UNWIND [1] AS n RETURN n"})
				AssertNoError(t, err)
			},
		},
		// Really big stream streaming
		{
			name: "Streaming big stream",
			fun: func(t *testing.T, c db.Connection) {
				_, err := c.Run(db.Command{Cypher: "UNWIND RANGE (0, 1000000) AS x RETURN x"}, db.TxConfig{Mode: db.ReadMode})
				AssertNoError(t, err)
			},
		},
		// Big nested streams in an uncommitted tx
		{
			name: "Streaming big stream",
			fun: func(t *testing.T, c db.Connection) {
				tx, err := c.TxBegin(db.TxConfig{Mode: db.WriteMode})
				_, err = c.RunTx(tx, db.Command{Cypher: "UNWIND RANGE (0, 1000000) AS n RETURN n"})
				AssertNoError(t, err)
				_, err = c.RunTx(tx, db.Command{Cypher: "UNWIND RANGE (0, 1000000) AS n RETURN n"})
				AssertNoError(t, err)
			},
		},
	}
	for _, c := range cases {
		ot.Run(c.name, func(t *testing.T) {
			c.fun(t, boltConn)
			if !boltConn.IsAlive() {
				t.Error("Connection died")
			}
			boltConn.Reset()
			// Should be working now
			s, err := boltConn.Run(db.Command{Cypher: "RETURN 42"}, db.TxConfig{Mode: db.ReadMode})
			AssertNoError(t, err)
			if s == nil {
				t.Fatal("Didn't get a stream")
			}
			boltConn.Next(s)
			boltConn.Next(s)
		})
	}
	// Run some of the above at random as one test
	ot.Run("Random reset sequence", func(t *testing.T) {
		randoms := make([]int, 25)
		for i := range randoms {
			randoms[i] = int(randInt() % int64(len(cases)))
		}
		for _, i := range randoms {
			c := cases[i]
			c.fun(t, boltConn)
			if !boltConn.IsAlive() {
				t.Error("Connection died")
			}
			boltConn.Reset()
		}
	})

	// Write really big query
	ot.Run("Really big query", func(t *testing.T) {
		query := "RETURN $x"
		bigBuilder := strings.Builder{}
		s := "0123456789"
		n := 100000
		size := len(s) * n // Should exceed 64k
		bigBuilder.Grow(size)
		for i := 0; i < n; i++ {
			bigBuilder.WriteString("0123456789")
		}

		stream, err := boltConn.Run(db.Command{Cypher: query, Params: map[string]interface{}{"x": bigBuilder.String()}}, db.TxConfig{Mode: db.ReadMode})
		AssertNoError(t, err)
		rec, sum, err := boltConn.Next(stream)
		AssertNextOnlyRecord(t, rec, sum, err)
		recS := rec.Values[0].(string)
		if recS != bigBuilder.String() {
			t.Errorf("Strings differ")
		}
		// Run the same thing once again to excerise buffer reuse at connection
		// level (there has been a bug caught by this).
		stream, err = boltConn.Run(db.Command{Cypher: query, Params: map[string]interface{}{"x": bigBuilder.String()}}, db.TxConfig{Mode: db.ReadMode})
		AssertNoError(t, err)
		rec, sum, err = boltConn.Next(stream)
		AssertNextOnlyRecord(t, rec, sum, err)
		recS = rec.Values[0].(string)
		if recS != bigBuilder.String() {
			t.Errorf("Strings differ")
		}
	})

	// Bookmark tests
	ot.Run("Bookmarks", func(tt *testing.T) {
		boltConn.Reset()
		lastBookmark := boltConn.Bookmark()

		assertNewBookmark := func(t *testing.T) {
			t.Helper()
			bookmark := boltConn.Bookmark()
			if len(bookmark) == 0 {
				t.Fatal("No bookmark")
			}
			if bookmark == lastBookmark {
				t.Fatal("No new bookmark")
			}
			lastBookmark = bookmark
		}

		assertNoNewBookmark := func(t *testing.T) {
			t.Helper()
			bookmark := boltConn.Bookmark()
			if bookmark != lastBookmark {
				t.Fatal("New bookmark")
			}
		}

		tt.Run("Auto-commit, bookmark by iteration", func(t *testing.T) {
			s, _ := boltConn.Run(db.Command{
				Cypher: "CREATE (n:BmRand {x: $rand}) RETURN n", Params: map[string]interface{}{"rand": randInt()}}, db.TxConfig{Mode: db.WriteMode})
			boltConn.Next(s)
			boltConn.Next(s)
			assertNewBookmark(t)
		})
		tt.Run("Auto-commit, bookmark by new auto-commit", func(t *testing.T) {
			boltConn.Run(db.Command{
				Cypher: "CREATE (n:BmRand {x: $rand}) RETURN n", Params: map[string]interface{}{"rand": randInt()}}, db.TxConfig{Mode: db.WriteMode})
			s, _ := boltConn.Run(db.Command{
				Cypher: "CREATE (n:BmRand {x: $rand}) RETURN n", Params: map[string]interface{}{"rand": randInt()}}, db.TxConfig{Mode: db.WriteMode})
			assertNewBookmark(t)
			boltConn.Next(s)
			boltConn.Next(s)
			assertNewBookmark(t)
		})
		tt.Run("Commit", func(t *testing.T) {
			tx, _ := boltConn.TxBegin(db.TxConfig{Mode: db.WriteMode})
			s, _ := boltConn.RunTx(tx, db.Command{
				Cypher: "CREATE (n:BmRand {x: $rand}) RETURN n", Params: map[string]interface{}{"rand": randInt()}})
			boltConn.Next(s)
			boltConn.Next(s)
			assertNoNewBookmark(t)
			boltConn.TxCommit(tx)
			assertNewBookmark(t)
		})
		tt.Run("Rollback", func(t *testing.T) {
			tx, _ := boltConn.TxBegin(db.TxConfig{Mode: db.WriteMode})
			s, _ := boltConn.RunTx(tx, db.Command{
				Cypher: "CREATE (n:BmRand {x: $rand}) RETURN n", Params: map[string]interface{}{"rand": randInt()}})
			boltConn.Next(s)
			boltConn.Next(s)
			assertNoNewBookmark(t)
			boltConn.TxRollback(tx)
			assertNoNewBookmark(t)
		})
	})

	// Enterprise feature
	ot.Run("Multidatabase", func(tt *testing.T) {
		selector, supportsMultidatabase := boltConn.(db.DatabaseSelector)
		if !supportsMultidatabase {
			tt.Skipf("Database %s:%s does not support multidatabase functionality", boltConn.ServerName(), boltConn.ServerVersion())
		}

		if !server.IsEnterprise {
			tt.Skip("Need enterprise edition to test multidatabase")
		}

		// Should always reset before selecting a database
		boltConn.Reset()
		// Connect to system database and create a test databases
		selector.SelectDatabase("system")
		boltConn.Run(db.Command{Cypher: "DROP DATABASE test1 IF EXISTS"}, db.TxConfig{Mode: db.WriteMode})
		_, err := boltConn.Run(db.Command{Cypher: "CREATE DATABASE test1"}, db.TxConfig{Mode: db.WriteMode})
		if err != nil {
			dbErr, _ := err.(*db.Neo4jError)
			if dbErr == nil || dbErr.Code != "Neo.ClientError.Database.ExistingDatabaseFound" {
				tt.Fatal(err)
			}
		}
		boltConn.Reset()
		// Use test database to create a random node
		selector.SelectDatabase("test1")
		r := randInt()
		_, err = boltConn.Run(db.Command{Cypher: "CREATE (n:MdbRand {x: $x}) RETURN n", Params: map[string]interface{}{"x": r}}, db.TxConfig{Mode: db.WriteMode})
		AssertNoError(tt, err)
		boltConn.Reset()
		// Connect to standard database and make sure we can't see the node
		s, err := boltConn.Run(db.Command{Cypher: "MATCH (n:MdbRand {x: $x}) RETURN n", Params: map[string]interface{}{"x": r}}, db.TxConfig{Mode: db.ReadMode})
		AssertNoError(tt, err)
		rec, sum, err := boltConn.Next(s)
		AssertNextOnlySummary(tt, rec, sum, err)
		boltConn.Reset()
		// Connect to test database and make sure we can see the node
		selector.SelectDatabase("test1")
		s, err = boltConn.Run(db.Command{Cypher: "MATCH (n:MdbRand {x: $x}) RETURN n", Params: map[string]interface{}{"x": r}}, db.TxConfig{Mode: db.ReadMode})
		AssertNoError(tt, err)
		rec, sum, err = boltConn.Next(s)
		AssertNextOnlyRecord(tt, rec, sum, err)
		boltConn.Reset()
	})
}
