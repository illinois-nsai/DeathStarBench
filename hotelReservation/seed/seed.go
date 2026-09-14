// Package seed writes a service's test data idempotently.
//
// Every replica runs initializeDatabase() in its main(), so an unconditional
// InsertMany gives N replicas N copies of every seed document. That is not a
// cosmetic problem: rate reads its whole collection with an empty filter, so at
// 7 replicas rate-db.inventory held 189 documents for 27 distinct hotels, its
// response grew 7x (5.7KB -> 42KB per request), and the inflated hotel list
// propagated into search and then into CheckAvailability, whose memcached
// multiget went from 374 to ~2700 keys. Replica count silently changed the
// workload, so search results were not comparable across configurations.
//
// Isolation run at 250 users, scaling one service at a time, confirmed rate as
// the only source: reservation x7 and geo x7 left gets/CheckAvailability at 378
// and 372, while rate x7 alone took it to 827 and the rate response to 41.5KB.
package seed

import (
	"context"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Doc is one seed record together with the filter identifying it uniquely.
type Doc struct {
	Filter bson.M
	Value  interface{}
}

// Upsert writes each document if absent and overwrites it if present, so the
// collection converges to one copy however many replicas run this.
func Upsert(ctx context.Context, collection *mongo.Collection, docs []Doc) error {
	opts := options.Update().SetUpsert(true)

	for _, doc := range docs {
		_, err := collection.UpdateOne(
			ctx,
			doc.Filter,
			bson.M{"$set": doc.Value},
			opts,
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// EnsureUniqueIndex makes the database enforce what Upsert assumes.
//
// Upsert alone is not enough: replicas start simultaneously, so two can both
// find no existing document and both insert one. The unique constraint turns
// that race into a duplicate-key error on the loser rather than a second copy.
//
// Only for collections that are read-only at runtime. reservation-db.reservation
// is deliberately excluded - MakeReservation inserts there, and the workload
// draws customers from a pool of 500, so repeat bookings of the same hotel and
// dates are legitimate rather than duplicates.
func EnsureUniqueIndex(ctx context.Context, collection *mongo.Collection, keys ...string) error {
	fields := bson.D{}
	for _, key := range keys {
		fields = append(fields, bson.E{Key: key, Value: 1})
	}

	_, err := collection.Indexes().CreateOne(
		ctx,
		mongo.IndexModel{
			Keys:    fields,
			Options: options.Index().SetUnique(true),
		},
	)

	return err
}
