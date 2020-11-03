package statestore

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/golang/protobuf/ptypes/timestamp"
	"github.com/gomodule/redigo/redis"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	utilTesting "open-match.dev/open-match/internal/util/testing"
	"open-match.dev/open-match/pkg/pb"
)

func TestCreateBackfill(t *testing.T) {
	cfg, closer := createRedis(t, false, "")
	defer closer()
	service := New(cfg)
	require.NotNil(t, service)
	defer service.Close()
	ctx := utilTesting.NewContext(t)

	var testCases = []struct {
		description     string
		backfill        *pb.Backfill
		expectedCode    codes.Code
		expectedMessage string
	}{
		{
			description: "ok",
			backfill: &pb.Backfill{
				Id:         "1",
				Generation: 1,
			},
			expectedCode:    codes.OK,
			expectedMessage: "",
		},
		{
			description:     "nil backfill passed, err expected",
			backfill:        nil,
			expectedCode:    codes.Internal,
			expectedMessage: "failed to marshal the backfill proto, id: : proto: Marshal called with nil",
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.description, func(t *testing.T) {
			err := service.CreateBackfill(ctx, tc.backfill)
			if tc.expectedCode == codes.OK {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.Equal(t, tc.expectedCode.String(), status.Convert(err).Code().String())
				require.Contains(t, status.Convert(err).Message(), tc.expectedMessage)
			}
		})
	}

	// pass an expired context, err expected
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	service = New(cfg)
	err := service.CreateBackfill(ctx, &pb.Backfill{
		Id: "222",
	})
	require.Error(t, err)
	require.Equal(t, codes.Unavailable.String(), status.Convert(err).Code().String())
	require.Contains(t, status.Convert(err).Message(), "CreateBackfill, id: 222, failed to connect to redis:")
}

func TestGetBackfill(t *testing.T) {
	cfg, closer := createRedis(t, false, "")
	defer closer()
	service := New(cfg)
	require.NotNil(t, service)
	defer service.Close()
	ctx := utilTesting.NewContext(t)

	expectedBackfill := &pb.Backfill{
		Id:         "mockBackfillID",
		Generation: 1,
	}
	err := service.CreateBackfill(ctx, expectedBackfill)
	require.NoError(t, err)

	c, err := redis.Dial("tcp", fmt.Sprintf("%s:%s", cfg.GetString("redis.hostname"), cfg.GetString("redis.port")))
	require.NoError(t, err)
	_, err = c.Do("SET", "wrong-type-key", "wrong-type-value")
	require.NoError(t, err)

	var testCases = []struct {
		description     string
		backfillID      string
		expectedCode    codes.Code
		expectedMessage string
	}{
		{
			description:     "backfill is found",
			backfillID:      "mockBackfillID",
			expectedCode:    codes.OK,
			expectedMessage: "",
		},
		{
			description:     "empty id passed, err expected",
			backfillID:      "",
			expectedCode:    codes.NotFound,
			expectedMessage: "Backfill id:  not found",
		},
		{
			description:     "wrong id passed, err expected",
			backfillID:      "123456",
			expectedCode:    codes.NotFound,
			expectedMessage: "Backfill id: 123456 not found",
		},
		{
			description:     "item of a wrong type is requested, err expected",
			backfillID:      "wrong-type-key",
			expectedCode:    codes.Internal,
			expectedMessage: "failed to unmarshal the backfill proto, id: wrong-type-key: proto: can't skip unknown wire type",
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.description, func(t *testing.T) {
			backfillActual, errActual := service.GetBackfill(ctx, tc.backfillID)
			if tc.expectedCode == codes.OK {
				require.NoError(t, errActual)
				require.NotNil(t, backfillActual)
				require.Equal(t, expectedBackfill.Id, backfillActual.Id)
				require.Equal(t, expectedBackfill.SearchFields, backfillActual.SearchFields)
				require.Equal(t, expectedBackfill.Extensions, backfillActual.Extensions)
				require.Equal(t, expectedBackfill.Generation, backfillActual.Generation)
			} else {
				require.Error(t, errActual)
				require.Equal(t, tc.expectedCode.String(), status.Convert(errActual).Code().String())
				require.Contains(t, status.Convert(errActual).Message(), tc.expectedMessage)
			}
		})
	}

	// pass an expired context, err expected
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	service = New(cfg)
	res, err := service.GetBackfill(ctx, "12345")
	require.Error(t, err)
	require.Nil(t, res)
	require.Equal(t, codes.Unavailable.String(), status.Convert(err).Code().String())
	require.Contains(t, status.Convert(err).Message(), "GetBackfill, id: 12345, failed to connect to redis:")
}

func TestDeleteBackfill(t *testing.T) {
	cfg, closer := createRedis(t, false, "")
	defer closer()
	service := New(cfg)
	require.NotNil(t, service)
	defer service.Close()
	ctx := utilTesting.NewContext(t)

	err := service.CreateBackfill(ctx, &pb.Backfill{
		Id:         "mockBackfillID",
		Generation: 1,
	})
	require.NoError(t, err)

	var testCases = []struct {
		description     string
		ticketID        string
		expectedCode    codes.Code
		expectedMessage string
	}{
		{
			description:     "backfill is found and deleted",
			ticketID:        "mockBackfillID",
			expectedCode:    codes.OK,
			expectedMessage: "",
		},
		{
			description:     "empty id passed, no err expected",
			ticketID:        "",
			expectedCode:    codes.OK,
			expectedMessage: "",
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.description, func(t *testing.T) {
			errActual := service.DeleteBackfill(ctx, tc.ticketID)
			if tc.expectedCode == codes.OK {
				require.NoError(t, errActual)

				if tc.ticketID != "" {
					_, errGetTicket := service.GetTicket(ctx, tc.ticketID)
					require.Error(t, errGetTicket)
					require.Equal(t, codes.NotFound.String(), status.Convert(errGetTicket).Code().String())
				}
			} else {
				require.Error(t, errActual)
				require.Equal(t, tc.expectedCode.String(), status.Convert(errActual).Code().String())
				require.Contains(t, status.Convert(errActual).Message(), tc.expectedMessage)
			}
		})
	}

	// pass an expired context, err expected
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	service = New(cfg)
	err = service.DeleteBackfill(ctx, "12345")
	require.Error(t, err)
	require.Equal(t, codes.Unavailable.String(), status.Convert(err).Code().String())
	require.Contains(t, status.Convert(err).Message(), "DeleteBackfill, id: 12345, failed to connect to redis:")
}

func TestUpdateBackfill(t *testing.T) {
	cfg, closer := createRedis(t, false, "")
	defer closer()
	service := New(cfg)
	require.NotNil(t, service)
	defer service.Close()
	ctx := utilTesting.NewContext(t)

	existingBackfill := &pb.Backfill{
		Id:         "mockBackfillID",
		Generation: 1,
		CreateTime: &timestamp.Timestamp{Seconds: 5},
	}
	err := service.CreateBackfill(ctx, existingBackfill)
	require.NoError(t, err)

	c, err := redis.Dial("tcp", fmt.Sprintf("%s:%s", cfg.GetString("redis.hostname"), cfg.GetString("redis.port")))
	require.NoError(t, err)
	_, err = c.Do("SET", "wrong-type-key", "wrong-type-value")
	require.NoError(t, err)

	updateFunc := func(current *pb.Backfill, new *pb.Backfill) (*pb.Backfill, error) {
		// custom logic
		if current.Generation == new.Generation {
			return new, nil
		}
		return nil, status.Error(codes.Internal, "can not update backfill with a different generation")
	}

	var testCases = []struct {
		description      string
		incomingBackfill *pb.Backfill
		updateFunc       func(current *pb.Backfill, new *pb.Backfill) (*pb.Backfill, error)
		resultBackfill   *pb.Backfill
		expectedCode     codes.Code
		expectedMessage  string
	}{
		{
			description: "ok update",
			incomingBackfill: &pb.Backfill{
				Id:         "mockBackfillID",
				Generation: 1,
				CreateTime: &timestamp.Timestamp{Seconds: 155},
			},
			updateFunc:      updateFunc,
			expectedCode:    codes.OK,
			expectedMessage: "",
		},
		{
			description: "no update, err returned",
			incomingBackfill: &pb.Backfill{
				Id:         "mockBackfillID",
				Generation: 2555,
				CreateTime: &timestamp.Timestamp{Seconds: 155},
			},
			updateFunc:      updateFunc,
			expectedCode:    codes.InvalidArgument,
			expectedMessage: "can not update backfill with a different generation",
		},
		{
			description: "nil updateFunc, err returned",
			incomingBackfill: &pb.Backfill{
				Id:         "mockBackfillID",
				Generation: 2555,
				CreateTime: &timestamp.Timestamp{Seconds: 155},
			},
			updateFunc:      nil,
			expectedCode:    codes.Internal,
			expectedMessage: "nil updateFunc provided",
		},
		{
			description: "wrong item requested, err returned",
			incomingBackfill: &pb.Backfill{
				Id:         "wrong-type-key",
				Generation: 2555,
				CreateTime: &timestamp.Timestamp{Seconds: 155},
			},
			updateFunc:      updateFunc,
			expectedCode:    codes.Internal,
			expectedMessage: "failed to unmarshal the backfill proto, id: wrong-type-key",
		},
		{
			description: "nil backfill returned from updateFunc, err returned",
			incomingBackfill: &pb.Backfill{
				Id:         "mockBackfillID",
				Generation: 2555,
				CreateTime: &timestamp.Timestamp{Seconds: 155},
			},
			updateFunc: func(current *pb.Backfill, new *pb.Backfill) (*pb.Backfill, error) {
				return nil, nil
			},
			expectedCode:    codes.Internal,
			expectedMessage: "failed to marshal the backfill proto, id: : proto: Marshal called with nil",
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.description, func(t *testing.T) {
			res, errActual := service.UpdateBackfill(ctx, tc.incomingBackfill, tc.updateFunc)
			if tc.expectedCode == codes.OK {
				require.NoError(t, errActual)
				require.NotNil(t, res)
				// generation fields are equall so we update existing backfill with a new one
				require.Equal(t, tc.incomingBackfill, res)
			} else {
				require.Error(t, errActual)
				require.Equal(t, tc.expectedCode.String(), status.Convert(errActual).Code().String())
				require.Contains(t, status.Convert(errActual).Message(), tc.expectedMessage)
				// generation fields are different, no changes expected
				require.Nil(t, res)
			}
		})
	}

	// pass an expired context, err expected
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	service = New(cfg)
	res, err := service.UpdateBackfill(ctx, existingBackfill, updateFunc)
	require.Error(t, err)
	require.Nil(t, res)
	require.Equal(t, codes.Unavailable.String(), status.Convert(err).Code().String())
	require.Contains(t, status.Convert(err).Message(), "UpdateBackfill, id: mockBackfillID, failed to connect to redis:")
}

func TestMapTicketsToBackfill(t *testing.T) {
	cfg, closer := createRedis(t, false, "")
	defer closer()
	service := New(cfg)
	require.NotNil(t, service)
	defer service.Close()
	ctx := utilTesting.NewContext(t)

	backfillID := "bf-id"
	generation := 5
	ticketIDs := []string{"id1", "id2"}

	c, err := redis.Dial("tcp", fmt.Sprintf("%s:%s", cfg.GetString("redis.hostname"), cfg.GetString("redis.port")))
	require.NoError(t, err)

	err = service.MapTicketsToBackfill(ctx, backfillID, generation, ticketIDs)
	require.NoError(t, err)

	res, err := redis.Bytes(c.Do("GET", "bf-id-5"))
	require.NoError(t, err)

	val := []string{}
	err = json.Unmarshal(res, &val)
	require.Equal(t, ticketIDs, val)

	// pass an expired context, err expected
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	service = New(cfg)
	err = service.MapTicketsToBackfill(ctx, backfillID, generation, ticketIDs)
	require.Error(t, err)
	require.Equal(t, codes.Unavailable.String(), status.Convert(err).Code().String())
	require.Contains(t, status.Convert(err).Message(), "MapTicketsToBackfill, key: bf-id-5, failed to connect to redis:")
}

func TestGetTicketIDsByBackfill(t *testing.T) {
	cfg, closer := createRedis(t, false, "")
	defer closer()
	service := New(cfg)
	require.NotNil(t, service)
	defer service.Close()
	ctx := utilTesting.NewContext(t)

	backfillID := "bf-id"
	generation := 5
	ticketIDs := []string{"id1", "id2"}
	val, err := json.Marshal(ticketIDs)
	require.NoError(t, err)

	c, err := redis.Dial("tcp", fmt.Sprintf("%s:%s", cfg.GetString("redis.hostname"), cfg.GetString("redis.port")))
	require.NoError(t, err)
	_, err = c.Do("SET", fmt.Sprintf("%s-%d", backfillID, generation), val)
	require.NoError(t, err)

	_, err = c.Do("SET", "wrong-type-key-123", "wrong-type-value")
	require.NoError(t, err)

	var testCases = []struct {
		description     string
		backfillID      string
		generation      int
		expectedResult  []string
		expectedCode    codes.Code
		expectedMessage string
	}{
		{
			description:     "OK",
			backfillID:      "bf-id",
			generation:      5,
			expectedResult:  ticketIDs,
			expectedCode:    codes.OK,
			expectedMessage: "",
		},
		{
			description:     "Wrong backfillID, err not found",
			backfillID:      "bf-q",
			generation:      5,
			expectedResult:  ticketIDs,
			expectedCode:    codes.NotFound,
			expectedMessage: "backfill mapping not found, key: bf-q-5",
		},
		{
			description:     "Wrong generation, err not found",
			backfillID:      "bf-id",
			generation:      123,
			expectedResult:  ticketIDs,
			expectedCode:    codes.NotFound,
			expectedMessage: "backfill mapping not found, key: bf-id-123",
		},
		{
			description:     "Retrieve wrong value, err not found",
			backfillID:      "wrong-type-key",
			generation:      123,
			expectedResult:  ticketIDs,
			expectedCode:    codes.Internal,
			expectedMessage: "failed to unmarshal the backfill mapping, key: wrong-type-key-123",
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.description, func(t *testing.T) {
			res, errActual := service.GetTicketIDsByBackfill(ctx, tc.backfillID, tc.generation)
			if tc.expectedCode == codes.OK {
				require.NoError(t, errActual)
				require.Equal(t, tc.expectedResult, res)

			} else {
				require.Error(t, errActual)
				require.Equal(t, tc.expectedCode.String(), status.Convert(errActual).Code().String())
				require.Contains(t, status.Convert(errActual).Message(), tc.expectedMessage)
			}
		})
	}
}
