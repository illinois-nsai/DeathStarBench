package main

import (
	"context"
	"fmt"
	"strconv"

	"github.com/rs/zerolog/log"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/bson"
	"github.com/delimitrou/DeathStarBench/tree/master/hotelReservation/seed"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type Reservation struct {
	HotelId      string `bson:"hotelId"`
	CustomerName string `bson:"customerName"`
	InDate       string `bson:"inDate"`
	OutDate      string `bson:"outDate"`
	Number       int    `bson:"number"`
}

type Number struct {
	HotelId string `bson:"hotelId"`
	Number  int    `bson:"numberOfRoom"`
}

// roomScale multiplies the seeded room counts.
//
// Stock capacity is 80 hotels / 19750 rooms, and the reserve workload books one
// room per night over a 1-5 night span, inserting one reservation document per
// night. Across the 15 bookable dates that is 296250 room-date slots, and a
// single 3-minute experiment consumes:
//
//     100 users ->  16%      500 users ->  79%
//     300 users ->  47%      700 users -> 111%  (exhausts mid-run)
//
// Once a hotel-date is full CheckAvailability returns early without inserting,
// so requests get cheaper as the run proceeds - and cheaper faster at higher
// load. That put a load-dependent bias straight into the latency label: in the
// 9/13 sweep reserve latency *fell* from 9.5ms at 400 users to 7.4ms at 700,
// and three identical back-to-back runs measured 37 / 19 / 16 ms.
//
// At 25x the top load level consumes ~4% of inventory instead of 111%, so every
// request does the same work and load is the only thing that varies.
const roomScale = 25

func initializeDatabase(url string) (*mongo.Client, func()) {
	log.Info().Msg("Generating test data...")

	newReservations := []interface{}{
		Reservation{"4", "Alice", "2015-04-09", "2015-04-10", 1},
	}

	newNumbers := []interface{}{
		Number{"1", 200 * roomScale},
		Number{"2", 200 * roomScale},
		Number{"3", 200 * roomScale},
		Number{"4", 200 * roomScale},
		Number{"5", 200 * roomScale},
		Number{"6", 200 * roomScale},
	}

	// The 200/250/300 spread is kept so hotels still differ in capacity; only
	// the absolute scale changes.
	for i := 7; i <= 80; i++ {
		hotelID := strconv.Itoa(i)

		roomNumber := 200
		if i%3 == 1 {
			roomNumber = 300
		} else if i%3 == 2 {
			roomNumber = 250
		}

		newNumbers = append(newNumbers, Number{hotelID, roomNumber * roomScale})
	}

	uri := fmt.Sprintf("mongodb://%s", url)
	log.Info().Msgf("Attempting connection to %v", uri)

	opts := options.Client().ApplyURI(uri)
	client, err := mongo.Connect(context.TODO(), opts)
	if err != nil {
		log.Panic().Msg(err.Error())
	}
	log.Info().Msg("Successfully connected to MongoDB")

	database := client.Database("reservation-db")
	resCollection := database.Collection("reservation")
	numCollection := database.Collection("number")

	// No unique index on reservation: MakeReservation inserts here at runtime and
	// the workload draws customers from a pool of 500, so two bookings of the
	// same hotel and dates by the same name are legitimate, not duplicates. The
	// upsert still keeps the single seed row from multiplying per replica.
	resDocs := make([]seed.Doc, 0, len(newReservations))
	for _, reservation := range newReservations {
		r := reservation.(Reservation)
		resDocs = append(resDocs, seed.Doc{
			Filter: bson.M{
				"hotelId":      r.HotelId,
				"customerName": r.CustomerName,
				"inDate":       r.InDate,
				"outDate":      r.OutDate,
			},
			Value: r,
		})
	}

	err = seed.Upsert(context.TODO(), resCollection, resDocs)
	if err != nil {
		log.Fatal().Msg(err.Error())
	}

	// number is read-only at runtime, so it can carry the constraint.
	if err := seed.EnsureUniqueIndex(context.TODO(), numCollection, "hotelId"); err != nil {
		log.Panic().Msg(err.Error())
	}

	numDocs := make([]seed.Doc, 0, len(newNumbers))
	for _, number := range newNumbers {
		n := number.(Number)
		numDocs = append(numDocs, seed.Doc{Filter: bson.M{"hotelId": n.HotelId}, Value: n})
	}

	err = seed.Upsert(context.TODO(), numCollection, numDocs)
	if err != nil {
		log.Fatal().Msg(err.Error())
	}
	log.Info().Msg("Successfully inserted test data into reservation DB")

	return client, func() {
		if err := client.Disconnect(context.TODO()); err != nil {
			log.Fatal().Msg(err.Error())
		}
	}
}
