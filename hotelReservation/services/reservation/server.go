package reservation

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	"github.com/delimitrou/DeathStarBench/tree/master/hotelReservation/registry"
	pb "github.com/delimitrou/DeathStarBench/tree/master/hotelReservation/services/reservation/proto"
	"github.com/delimitrou/DeathStarBench/tree/master/hotelReservation/tls"
	"github.com/google/uuid"
	"github.com/grpc-ecosystem/grpc-opentracing/go/otgrpc"
	"github.com/opentracing/opentracing-go"
	"github.com/rs/zerolog/log"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

const name = "srv-reservation"

// Server implements the user service
type Server struct {
	pb.UnimplementedReservationServer

	uuid string

	Tracer      opentracing.Tracer
	Port        int
	IpAddr      string
	MongoClient *mongo.Client
	Registry    *registry.Client
	MemcClient  *memcache.Client
}

// Run starts the server
func (s *Server) Run() error {
	opentracing.SetGlobalTracer(s.Tracer)

	if s.Port == 0 {
		return fmt.Errorf("server port must be set")
	}

	s.uuid = uuid.New().String()

	opts := []grpc.ServerOption{
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Timeout: 120 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			PermitWithoutStream: true,
		}),
		grpc.UnaryInterceptor(
			otgrpc.OpenTracingServerInterceptor(s.Tracer),
		),
	}

	if tlsopt := tls.GetServerOpt(); tlsopt != nil {
		opts = append(opts, tlsopt)
	}

	srv := grpc.NewServer(opts...)

	pb.RegisterReservationServer(srv, s)

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", s.Port))
	if err != nil {
		log.Fatal().Msgf("failed to listen: %v", err)
	}

	log.Trace().Msgf("In reservation s.IpAddr = %s, port = %d", s.IpAddr, s.Port)

	err = s.Registry.Register(name, s.uuid, s.IpAddr, s.Port)
	if err != nil {
		return fmt.Errorf("failed register: %v", err)
	}
	log.Info().Msg("Successfully registered in consul")

	return srv.Serve(lis)
}

// Shutdown cleans up any processes
func (s *Server) Shutdown() {
	s.Registry.Deregister(s.uuid)
}

// MakeReservation makes a reservation based on given information
func (s *Server) MakeReservation(ctx context.Context, req *pb.Request) (*pb.Result, error) {
	res := new(pb.Result)
	res.HotelId = make([]string, 0)

	database := s.MongoClient.Database("reservation-db")
	resCollection := database.Collection("reservation")
	numCollection := database.Collection("number")

	inDate, _ := time.Parse(
		time.RFC3339,
		req.InDate+"T12:00:00+00:00")

	outDate, _ := time.Parse(
		time.RFC3339,
		req.OutDate+"T12:00:00+00:00")
	hotelId := req.HotelId[0]

	indate := inDate.String()[0:10]

	memc_date_num_map := make(map[string]int)

	for inDate.Before(outDate) {
		// check reservations
		count := 0
		inDate = inDate.AddDate(0, 0, 1)
		outdate := inDate.String()[0:10]

		// first check memc
		memc_key := hotelId + "_" + inDate.String()[0:10] + "_" + outdate
		item, err := s.MemcClient.Get(memc_key)
		if err == nil {
			// memcached hit
			count, _ = strconv.Atoi(string(item.Value))
			log.Trace().Msgf("memcached hit %s = %d", memc_key, count)
			memc_date_num_map[memc_key] = count + int(req.RoomNumber)

		} else {
			// memcached miss, or any other memcached failure - a timeout, a reset
			// connection. Both fall through to mongo. Upstream panicked on
			// anything but ErrCacheMiss, so one slow cache read killed the whole
			// process and every request in flight with it.
			if err != memcache.ErrCacheMiss {
				log.Warn().Msgf("Tried to get memc_key [%v], but got memmcached error = %s; treating as a cache miss", memc_key, err)
			}
			log.Trace().Msgf("memcached miss")
			var reserve []reservation

			filter := bson.D{{"hotelId", hotelId}, {"inDate", indate}, {"outDate", outdate}}
			// Find returns a nil cursor with its error; calling All on it panics.
			curr, err := resCollection.Find(context.TODO(), filter)
			if err != nil {
				log.Error().Msgf("Failed to query reservations for hotel %v %v-%v: %v", hotelId, indate, outdate, err)
			} else if err := curr.All(context.TODO(), &reserve); err != nil {
				log.Error().Msgf("Failed to decode reservations for hotel %v %v-%v: %v", hotelId, indate, outdate, err)
			}

			for _, r := range reserve {
				count += r.Number
			}

			memc_date_num_map[memc_key] = count + int(req.RoomNumber)
		}

		// check capacity
		// check memc capacity
		memc_cap_key := hotelId + "_cap"
		item, err = s.MemcClient.Get(memc_cap_key)
		hotel_cap := 0
		if err == nil {
			// memcached hit
			hotel_cap, _ = strconv.Atoi(string(item.Value))
			log.Trace().Msgf("memcached hit %s = %d", memc_cap_key, hotel_cap)
		} else {
			// memcached miss or failure; see the note above.
			if err != memcache.ErrCacheMiss {
				log.Warn().Msgf("Tried to get memc_cap_key [%v], but got memmcached error = %s; treating as a cache miss", memc_cap_key, err)
			}
			var num number
			err = numCollection.FindOne(context.TODO(), &bson.D{{"hotelId", hotelId}}).Decode(&num)
			if err != nil {
				// Fail this request, not the process. Capacity is unknown here, so
				// the booking cannot be admitted safely - but panicking took down
				// the pod and every in-flight reservation with it. See the note on
				// the InsertOne below for what that cost.
				log.Error().Msgf("Failed to get capacity for hotel %v: %v", hotelId, err)
				return nil, fmt.Errorf("capacity lookup failed for hotel %v: %w", hotelId, err)
			}
			hotel_cap = int(num.Number)

			// write to memcache
			s.MemcClient.Set(&memcache.Item{Key: memc_cap_key, Value: []byte(strconv.Itoa(hotel_cap))})
		}

		if count+int(req.RoomNumber) > hotel_cap {
			return res, nil
		}
		indate = outdate
	}

	// only update reservation number cache after check succeeds
	for key, val := range memc_date_num_map {
		s.MemcClient.Set(&memcache.Item{Key: key, Value: []byte(strconv.Itoa(val))})
	}

	inDate, _ = time.Parse(
		time.RFC3339,
		req.InDate+"T12:00:00+00:00")

	indate = inDate.String()[0:10]

	for inDate.Before(outDate) {
		inDate = inDate.AddDate(0, 0, 1)
		outdate := inDate.String()[0:10]
		_, err := resCollection.InsertOne(
			context.TODO(),
			reservation{
				HotelId:      hotelId,
				CustomerName: req.CustomerName,
				InDate:       indate,
				OutDate:      outdate,
				Number:       int(req.RoomNumber),
			},
		)
		if err != nil {
			// Fail this request, not the process.
			//
			// This is the hottest write in the benchmark - one insert per booked
			// night, up to 15 per request - and panicking here crash-looped the
			// whole deployment under load. At 7 replicas driving a single
			// mongodb-reservation, roughly 700 driver operations contend for one
			// mongod; inserts start timing out, and every timeout killed a pod.
			// Measured in the 9/15 sweep: the app container was destroyed and
			// recreated ~5 times per pod per 3-minute run, its sidecar then could
			// not reach it on localhost, and 12-70% of requests failed with
			// grpc=14 / URX,UF. All ten failures were reserve at 7 replicas; the
			// same mix at 3 replicas and search at 7 were both stable.
			//
			// Earlier nights in this loop are already committed - there is no
			// transaction here - so returning leaves a partial reservation. That
			// is pre-existing, and reporting the failure beats both crashing and
			// silently claiming success.
			log.Error().Msgf("Failed to insert reservation for hotel %v %v-%v: %v", hotelId, indate, outdate, err)
			return nil, fmt.Errorf("reservation insert failed for hotel %v: %w", hotelId, err)
		}
		indate = outdate
	}

	res.HotelId = append(res.HotelId, hotelId)

	return res, nil
}

// CheckAvailability checks if given information is available
func (s *Server) CheckAvailability(ctx context.Context, req *pb.Request) (*pb.Result, error) {
	res := new(pb.Result)
	res.HotelId = make([]string, 0)

	hotelMemKeys := []string{}
	keysMap := make(map[string]struct{})
	resMap := make(map[string]bool)
	// cache capacity since it will not change
	for _, hotelId := range req.HotelId {
		hotelMemKeys = append(hotelMemKeys, hotelId+"_cap")
		resMap[hotelId] = true
		keysMap[hotelId+"_cap"] = struct{}{}
	}

	capMemSpan, _ := opentracing.StartSpanFromContext(ctx, "memcached_capacity_get_multi_number")
	capMemSpan.SetTag("span.kind", "client")
	cacheMemRes, err := s.MemcClient.GetMulti(hotelMemKeys)
	capMemSpan.Finish()

	numCollection := s.MongoClient.Database("reservation-db").Collection("number")

	misKeys := []string{}
	if err != nil && err != memcache.ErrCacheMiss {
		log.Warn().Msgf("Tried to get memc_cap_key [%v], but got memmcached error = %s; treating missing keys as cache misses", hotelMemKeys, err)
	}

	// Gather cache-miss keys to query in mongodb. This runs unconditionally,
	// including when err == nil: GetMulti reports a partial hit as a nil error
	// with only the keys it found, so absence from cacheMemRes is the only
	// reliable signal. On a timeout cacheMemRes is nil, and reading a nil map is
	// safe in Go, so the same loop then collects every requested key.
	for key := range keysMap {
		if _, ok := cacheMemRes[key]; !ok {
			misKeys = append(misKeys, key)
		}
	}
	// store whole capacity result in cacheCap
	cacheCap := make(map[string]int)
	for k, v := range cacheMemRes {
		hotelCap, _ := strconv.Atoi(string(v.Value))
		// Key by the bare hotelId. cacheMemRes is keyed "<hotelId>_cap", while the
		// mongo path below stores num.HotelId and both readers look up the bare
		// id, so storing k verbatim made every cache-served capacity unreachable:
		// the lookup missed, Go returned 0, and `count+RoomNumber <= 0` marked the
		// hotel unavailable. With the capacity cache at a ~100% hit rate that
		// emptied CheckAvailability's result, so search returned no hotels and
		// GetProfiles was called with an empty id list.
		cacheCap[strings.TrimSuffix(k, "_cap")] = hotelCap
	}
	if len(misKeys) > 0 {
		queryMissKeys := []string{}
		for _, k := range misKeys {
			queryMissKeys = append(queryMissKeys, strings.Split(k, "_")[0])
		}
		var nums []number
		capMongoSpan, _ := opentracing.StartSpanFromContext(ctx, "mongodb_capacity_get_multi_number")
		capMongoSpan.SetTag("span.kind", "client")
		// Two upstream bugs, both only reachable now that misKeys is populated
		// correctly - previously this whole block was dead code because the miss
		// list was only filled on memcache.ErrCacheMiss, which GetMulti never
		// returns.
		//
		// 1. bson.D{{"$in", ...}} is not a filter. $in is an operator and has to
		//    sit under the field it constrains, so mongo rejected the query.
		// 2. Find returns a nil cursor alongside its error, and the error was
		//    only logged before curr.All was called on it regardless - a nil
		//    dereference that took the whole process down:
		//       mongo.(*Cursor).All(0x0, ...) services/reservation/server.go:279
		filter := bson.D{{"hotelId", bson.D{{"$in", queryMissKeys}}}}
		curr, err := numCollection.Find(context.TODO(), filter)
		if err != nil {
			log.Error().Msgf("Failed to query reservation capacity for %v: %v", misKeys, err)
		} else if err := curr.All(context.TODO(), &nums); err != nil {
			log.Error().Msgf("Failed to decode reservation capacity for %v: %v", misKeys, err)
		}
		capMongoSpan.Finish()
		for _, num := range nums {
			cacheCap[num.HotelId] = num.Number
			// we don't care set successfully or not
			go s.MemcClient.Set(&memcache.Item{Key: num.HotelId + "_cap", Value: []byte(strconv.Itoa(num.Number))})
		}
	}

	reqCommand := []string{}
	queryMap := make(map[string]map[string]string)
	for _, hotelId := range req.HotelId {
		log.Trace().Msgf("reservation check hotel %s", hotelId)
		inDate, _ := time.Parse(
			time.RFC3339,
			req.InDate+"T12:00:00+00:00")
		outDate, _ := time.Parse(
			time.RFC3339,
			req.OutDate+"T12:00:00+00:00")
		for inDate.Before(outDate) {
			indate := inDate.String()[:10]
			inDate = inDate.AddDate(0, 0, 1)
			outDate := inDate.String()[:10]
			memcKey := hotelId + "_" + outDate + "_" + outDate
			reqCommand = append(reqCommand, memcKey)
			queryMap[memcKey] = map[string]string{
				"hotelId":   hotelId,
				"startDate": indate,
				"endDate":   outDate,
			}
		}
	}

	type taskRes struct {
		hotelId  string
		checkRes bool
	}
	reserveMemSpan, _ := opentracing.StartSpanFromContext(ctx, "memcached_reserve_get_multi_number")
	ch := make(chan taskRes)
	reserveMemSpan.SetTag("span.kind", "client")
	// check capacity in memcached and mongodb
	itemsMap, err := s.MemcClient.GetMulti(reqCommand)
	reserveMemSpan.Finish()
	// A memcached failure is handled exactly like a cache miss. Note the channel
	// close below: `ch` is closed either by the hit goroutine when err == nil or
	// by the WaitGroup goroutine on the miss path. A timeout matched neither, so
	// without routing it to the miss path nothing would ever close `ch` and the
	// consumer would block forever.
	if err != nil && err != memcache.ErrCacheMiss {
		log.Warn().Msgf("Tried to get memc_key [%v], but got memmcached error = %s; treating as a cache miss", reqCommand, err)
	}

	// Cache hits are answered straight from itemsMap; whatever is left in queryMap
	// once those are removed has to come from mongo. Both groups of senders are
	// tracked by one WaitGroup, so `ch` is closed exactly once, after the last
	// send.
	//
	// Upstream closed `ch` from the hit goroutine when err == nil and from the
	// mongo path when err == ErrCacheMiss. That left three holes: a partial hit
	// (nil error, only some keys returned) never consulted mongo; a memcached
	// timeout matched neither branch so nothing closed the channel and the
	// consumer blocked forever; and when both paths had work, the mongo path
	// could close `ch` while the hit goroutine was still sending to it.
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for k, v := range itemsMap {
			id := strings.Split(k, "_")[0]
			val, _ := strconv.Atoi(string(v.Value))
			var res bool
			if val+int(req.RoomNumber) <= cacheCap[id] {
				res = true
			}
			ch <- taskRes{
				hotelId:  id,
				checkRes: res,
			}
		}
	}()

	// Remove cache hits from queryMap; whatever remains must go to mongo.
	// rever string to indata and outdate
	for k := range itemsMap {
		delete(queryMap, k)
	}
	wg.Add(len(queryMap))
	for command := range queryMap {
		go func(comm string) {
			defer wg.Done()

			var reserve []reservation

			queryItem := queryMap[comm]
			resCollection := s.MongoClient.Database("reservation-db").Collection("reservation")
			filter := bson.D{{"hotelId", queryItem["hotelId"]}, {"inDate", queryItem["startDate"]}, {"outDate", queryItem["endDate"]}}

			reserveMongoSpan, _ := opentracing.StartSpanFromContext(ctx, "mongodb_capacity_get_multi_number"+comm)
			reserveMongoSpan.SetTag("span.kind", "client")
			// Same nil-cursor pattern as above.
			curr, err := resCollection.Find(context.TODO(), filter)
			if err != nil {
				log.Error().Msgf("Failed to query reservations for %v: %v", queryItem, err)
			} else if err := curr.All(context.TODO(), &reserve); err != nil {
				log.Error().Msgf("Failed to decode reservations for %v: %v", queryItem, err)
			}
			reserveMongoSpan.Finish()
			var count int
			for _, r := range reserve {
				log.Trace().Msgf("reservation check reservation number = %d", queryItem["hotelId"])
				count += r.Number
			}
			// update memcached
			go s.MemcClient.Set(&memcache.Item{Key: comm, Value: []byte(strconv.Itoa(count))})
			var res bool
			if count+int(req.RoomNumber) <= cacheCap[queryItem["hotelId"]] {
				res = true
			}
			ch <- taskRes{
				hotelId:  queryItem["hotelId"],
				checkRes: res,
			}
		}(command)
	}
	go func() {
		wg.Wait()
		close(ch)
	}()

	for task := range ch {
		if !task.checkRes {
			resMap[task.hotelId] = false
		}
	}
	for k, v := range resMap {
		if v {
			res.HotelId = append(res.HotelId, k)
		}
	}

	return res, nil
}

type reservation struct {
	HotelId      string `bson:"hotelId"`
	CustomerName string `bson:"customerName"`
	InDate       string `bson:"inDate"`
	OutDate      string `bson:"outDate"`
	Number       int    `bson:"number"`
}

type number struct {
	HotelId string `bson:"hotelId"`
	Number  int    `bson:"numberOfRoom"`
}
