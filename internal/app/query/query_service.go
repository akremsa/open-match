// Copyright 2019 Google LLC
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

package query

import (
	"context"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"go.opencensus.io/stats"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"open-match.dev/open-match/internal/appmain"
	"open-match.dev/open-match/internal/config"
	"open-match.dev/open-match/internal/filter"
	"open-match.dev/open-match/internal/statestore"
	"open-match.dev/open-match/pkg/pb"
)

var (
	logger = logrus.WithFields(logrus.Fields{
		"app":       "openmatch",
		"component": "app.query",
	})
)

type updater interface {
	filter()
	update() error
}

// queryService API provides utility functions for common MMF functionality such
// as retreiving Tickets from state storage.
type queryService struct {
	cfg   config.View
	cashe *cache
}

func (s *queryService) QueryTickets(req *pb.QueryTicketsRequest, responseServer pb.QueryService_QueryTicketsServer) error {
	ctx := responseServer.Context()
	pool := req.GetPool()
	if pool == nil {
		return status.Error(codes.InvalidArgument, ".pool is required")
	}

	pf, err := filter.NewPoolFilter(pool)
	if err != nil {
		return err
	}

	s.cashe.ticketHandler.pf = pf

	err = s.cashe.request(ctx, s.cashe.ticketHandler)
	if err != nil {
		err = errors.Wrap(err, "QueryTickets: failed to run request")
		return err
	}

	results := s.cashe.ticketHandler.filteredTickets
	stats.Record(ctx, ticketsPerQuery.M(int64(len(results))))

	pSize := getPageSize(s.cfg)
	for start := 0; start < len(results); start += pSize {
		end := start + pSize
		if end > len(results) {
			end = len(results)
		}

		err := responseServer.Send(&pb.QueryTicketsResponse{
			Tickets: results[start:end],
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *queryService) QueryTicketIds(req *pb.QueryTicketIdsRequest, responseServer pb.QueryService_QueryTicketIdsServer) error {
	return nil
}

func (s *queryService) QueryBackfills(req *pb.QueryBackfillsRequest, responseServer pb.QueryService_QueryBackfillsServer) error {
	return nil
}

func getPageSize(cfg config.View) int {
	const (
		name = "queryPageSize"
		// Minimum number of tickets to be returned in a streamed response for QueryTickets. This value
		// will be used if page size is configured lower than the minimum value.
		minPageSize int = 10
		// Default number of tickets to be returned in a streamed response for QueryTickets.  This value
		// will be used if page size is not configured.
		defaultPageSize int = 1000
		// Maximum number of tickets to be returned in a streamed response for QueryTickets. This value
		// will be used if page size is configured higher than the maximum value.
		maxPageSize int = 10000
	)

	if !cfg.IsSet(name) {
		return defaultPageSize
	}

	pSize := cfg.GetInt(name)
	if pSize < minPageSize {
		logger.Infof("page size %v is lower than the minimum limit of %v", pSize, maxPageSize)
		pSize = minPageSize
	}

	if pSize > maxPageSize {
		logger.Infof("page size %v is higher than the maximum limit of %v", pSize, maxPageSize)
		return maxPageSize
	}

	return pSize
}

/////////////////////////////////////////////////////////////////////
/////////////////////////////////////////////////////////////////////

// ticketCache unifies concurrent requests into a single cache update, and
// gives a safe view into that map cache.
type cache struct {
	requests chan *cacheRequest

	// Single item buffered channel.  Holds a value when runQuery can be safely
	// started.  Basically a channel/select friendly mutex around runQuery
	// running.
	startRunRequest chan struct{}

	wg sync.WaitGroup

	err error

	ticketHandler *ticketHandler
}

type ticketHandler struct {
	store           statestore.Service
	tickets         map[string]*pb.Ticket
	pf              *filter.PoolFilter
	filteredTickets []*pb.Ticket
}

func newCache(b *appmain.Bindings, cfg config.View) *cache {
	store := statestore.New(cfg)
	с := &cache{
		requests:        make(chan *cacheRequest),
		startRunRequest: make(chan struct{}, 1),
		ticketHandler: &ticketHandler{
			store:   store,
			tickets: make(map[string]*pb.Ticket),
		},
	}

	с.startRunRequest <- struct{}{}
	b.AddHealthCheckFunc(store.HealthCheck)

	return с
}

func (th *ticketHandler) filter() {
	th.filteredTickets = []*pb.Ticket{}
	for _, ticket := range th.tickets {
		if th.pf.In(ticket) {
			th.filteredTickets = append(th.filteredTickets, ticket)
		}
	}
}

func (th *ticketHandler) update() error {
	st := time.Now()
	previousCount := len(th.tickets)

	currentAll, err := th.store.GetIndexedIDSet(context.Background())
	if err != nil {
		return err
	}

	deletedCount := 0
	for id := range th.tickets {
		if _, ok := currentAll[id]; !ok {
			delete(th.tickets, id)
			deletedCount++
		}
	}

	toFetch := []string{}

	for id := range currentAll {
		if _, ok := th.tickets[id]; !ok {
			toFetch = append(toFetch, id)
		}
	}

	newTickets, err := th.store.GetTickets(context.Background(), toFetch)
	if err != nil {
		return err
	}

	for _, t := range newTickets {
		th.tickets[t.Id] = t
	}

	stats.Record(context.Background(), cacheTotalItems.M(int64(previousCount)))
	stats.Record(context.Background(), cacheFetchedItems.M(int64(len(toFetch))))
	stats.Record(context.Background(), cacheUpdateLatency.M(float64(time.Since(st))/float64(time.Millisecond)))

	logger.Debugf("Ticket Cache update: Previous %d, Deleted %d, Fetched %d, Current %d", previousCount, deletedCount, len(toFetch), len(th.tickets))
	return nil
}

type cacheRequest struct {
	ctx    context.Context
	runNow chan struct{}
}

func (c *cache) request(ctx context.Context, updater updater) error {
	cr := &cacheRequest{
		ctx:    ctx,
		runNow: make(chan struct{}),
	}

sendRequest:
	for {
		select {
		case <-ctx.Done():
			return errors.Wrap(ctx.Err(), "ticket cache request canceled before reuest sent.")
		case <-c.startRunRequest:
			go c.runRequest(updater)
		case c.requests <- cr:
			break sendRequest
		}
	}

	select {
	case <-ctx.Done():
		return errors.Wrap(ctx.Err(), "ticket cache request canceled waiting for access.")
	case <-cr.runNow:
		defer c.wg.Done()
	}

	if c.err != nil {
		return c.err
	}

	updater.filter()
	return nil
}

func (c *cache) runRequest(u updater) {
	defer func() {
		c.startRunRequest <- struct{}{}
	}()

	// Wait for first query request.
	reqs := []*cacheRequest{<-c.requests}

	// Collect all waiting queries.
collectAllWaiting:
	for {
		select {
		case req := <-c.requests:
			reqs = append(reqs, req)
		default:
			break collectAllWaiting
		}
	}

	c.err = u.update()

	stats.Record(context.Background(), cacheWaitingQueries.M(int64(len(reqs))))

	// Send WaitGroup to query calls, letting them run their query on the ticket
	// cache.
	for _, req := range reqs {
		c.wg.Add(1)
		select {
		case req.runNow <- struct{}{}:
		case <-req.ctx.Done():
			c.wg.Done()
		}
	}

	// wait for requests to finish using ticket cache.
	c.wg.Wait()
}

type backfillCache struct {
	store statestore.Service

	wg sync.WaitGroup

	// backfill cache
	backfills map[string]*pb.Backfill

	// startRunRequest acts as a semaphore indicator for defining
	// when the backfillCache is being used (queried) or is it in an idle state.
	startRunRequest chan struct{}

	// requests is the channel to handle all incoming query requests and
	// make only one request to the storage service.
	requests chan *cacheRequest

	// This error is used for the return value when a request fails to reach
	// the storage. In case of multiple requests are being handled, this error
	// is returned to every single request.
	err error
}

func newBackfillCache(b *appmain.Bindings, cfg config.View) *backfillCache {
	bc := &backfillCache{
		store:           statestore.New(cfg),
		startRunRequest: make(chan struct{}, 1),
		requests:        make(chan *cacheRequest),
		backfills:       make(map[string]*pb.Backfill),
	}

	bc.startRunRequest <- struct{}{}
	b.AddHealthCheckFunc(bc.store.HealthCheck)

	return bc
}

func (bc *backfillCache) requestBackfills(ctx context.Context, f func(map[string]*pb.Backfill)) error {
	cr := &cacheRequest{
		ctx:    ctx,
		runNow: make(chan struct{}),
	}

sendRequest:
	for {
		select {
		case <-ctx.Done():
			return errors.Wrap(ctx.Err(), "backfill cache request canceled before request sent.")
		case <-bc.startRunRequest:
			go bc.runRequest()
		case bc.requests <- cr:
			break sendRequest
		}
	}

	select {
	case <-ctx.Done():
		return errors.Wrap(ctx.Err(), "backfill cache request canceled waiting for access.")
	case <-cr.runNow:
		defer bc.wg.Done()
	}

	if bc.err != nil {
		return bc.err
	}

	f(bc.backfills)
	return nil
}

func (bc *backfillCache) runRequest() {
	defer func() {
		bc.startRunRequest <- struct{}{}
	}()

	// Wait for first query request.
	reqs := []*cacheRequest{<-bc.requests}

	// Collect all waiting queries.
collectAllWaiting:
	for {
		select {
		case req := <-bc.requests:
			reqs = append(reqs, req)
		default:
			break collectAllWaiting
		}
	}

	bc.update()
	stats.Record(context.Background(), cacheWaitingQueries.M(int64(len(reqs))))

	// Send WaitGroup to query calls, letting them run their query on the backfill cache.
	for _, req := range reqs {
		select {
		case req.runNow <- struct{}{}:
			bc.wg.Add(1)
		case <-req.ctx.Done():
		}
	}

	// wait for requests to finish using backfill cache.
	bc.wg.Wait()
}

func (bc *backfillCache) update() {
	previousCount := len(bc.backfills)

	indexedBackfills, err := bc.store.GetIndexedBackfills(context.Background())
	if err != nil {
		bc.err = err
		return
	}

	deletedCount := 0
	for id, cachedBackfill := range bc.backfills {
		indexedBackfillGeneration, ok := indexedBackfills[id]
		if !ok || cachedBackfill.Generation < int64(indexedBackfillGeneration) {
			delete(bc.backfills, id)
			deletedCount++
		}
	}

	toFetch := []string{}
	for id := range indexedBackfills {
		if _, ok := bc.backfills[id]; !ok {
			toFetch = append(toFetch, id)
		}
	}

	for _, backfillToFetch := range toFetch {
		storedBackfill, _, err := bc.store.GetBackfill(context.Background(), backfillToFetch)
		if err != nil {
			bc.err = err
			return
		}

		bc.backfills[storedBackfill.Id] = storedBackfill
	}

	logger.Debugf("Backfill Cache update: Previous %d, Deleted %d, Fetched %d, Current %d", previousCount, deletedCount, len(toFetch), len(bc.backfills))
	bc.err = nil
}
