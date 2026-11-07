// Package testdb gives tests an isolated MongoDB database with all indexes created.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/ashmit/reachout-server/internal/mdb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

const defaultURI = "mongodb://localhost:27017/?replicaSet=rs0"

var probe struct {
	once sync.Once
	err  error
}

// New creates a throwaway database and drops it when the test ends. The server comes from
// TEST_MONGO_URI (it must be a replica set, for transactions). If that variable is set and
// the server cannot be reached the test fails; only when it is unset and the default local
// server is absent is the test skipped, so a mistyped URI can never look like a pass.
func New(t testing.TB) *mongo.Database {
	t.Helper()
	uri, explicit := os.LookupEnv("TEST_MONGO_URI")
	if uri == "" {
		uri, explicit = defaultURI, false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	probe.once.Do(func() {
		pctx, pcancel := context.WithTimeout(ctx, 4*time.Second)
		defer pcancel()
		db, err := mdb.Open(pctx, uri, "reachout_probe")
		if err == nil {
			db.Client().Disconnect(context.Background())
		}
		probe.err = err
	})
	if probe.err != nil {
		if explicit {
			t.Fatalf("TEST_MONGO_URI is set but mongodb is not reachable: %v", probe.err)
		}
		t.Skipf("mongodb not available at %s (set TEST_MONGO_URI): %v", defaultURI, probe.err)
	}

	var b [6]byte
	rand.Read(b[:])
	db, err := mdb.Open(ctx, uri, "reachout_test_"+hex.EncodeToString(b[:]))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		db.Drop(c)
		db.Client().Disconnect(c)
	})
	if err := mdb.EnsureIndexes(ctx, db); err != nil {
		t.Fatalf("create indexes: %v", err)
	}
	return db
}

// Count returns how many documents in coll match filter.
func Count(t testing.TB, db *mongo.Database, coll string, filter any) int {
	t.Helper()
	n, err := db.Collection(coll).CountDocuments(context.Background(), filter)
	if err != nil {
		t.Fatal(err)
	}
	return int(n)
}

// One decodes the first document in coll matching filter, and fails the test if there is none.
func One[T any](t testing.TB, db *mongo.Database, coll string, filter any) T {
	t.Helper()
	var v T
	if err := db.Collection(coll).FindOne(context.Background(), filter).Decode(&v); err != nil {
		t.Fatalf("find in %s: %v", coll, err)
	}
	return v
}

// Insert adds a document, failing the test on error.
func Insert(t testing.TB, db *mongo.Database, coll string, doc any) {
	t.Helper()
	if _, err := db.Collection(coll).InsertOne(context.Background(), doc); err != nil {
		t.Fatalf("insert into %s: %v", coll, err)
	}
}

// Update applies $set-style changes to every document matching filter and returns how many matched.
func Update(t testing.TB, db *mongo.Database, coll string, filter, set any) int {
	t.Helper()
	r, err := db.Collection(coll).UpdateMany(context.Background(), filter, bson.D{{Key: "$set", Value: set}})
	if err != nil {
		t.Fatalf("update %s: %v", coll, err)
	}
	return int(r.MatchedCount)
}
