package kafka

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"sync/atomic"
	"testing"
	"time"

	watermillmessage "github.com/ThreeDotsLabs/watermill/message"
	"github.com/asyncapi/event-gateway/message"
	"github.com/asyncapi/event-gateway/proxy"
	kafkaproxy "github.com/grepplabs/kafka-proxy/proxy"
	kafkaprotocol "github.com/grepplabs/kafka-proxy/proxy/protocol"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewKafka(t *testing.T) {
	tests := []struct {
		name        string
		c           *ProxyConfig
		expectedErr error
	}{
		{
			name: "Proxy is created when config is valid",
			c: &ProxyConfig{
				BrokersMapping: []string{"localhost:9092, localhost:28002"},
			},
		},
		{
			name:        "Proxy creation errors when config is invalid",
			c:           &ProxyConfig{},
			expectedErr: errors.New("BrokersMapping is mandatory"),
		},
		{
			name:        "Proxy creation errors when config is missing",
			expectedErr: errors.New("config should be provided"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p, err := NewProxy(test.c)
			if test.expectedErr != nil {
				assert.EqualError(t, err, test.expectedErr.Error())
				assert.Nil(t, p)
			} else {
				assert.NoError(t, err)
				assert.IsType(t, (proxy.Proxy)(nil), p)
			}
		})
	}
}

func TestProduceRequestHandler_Handle(t *testing.T) {
	tests := []struct {
		name              string
		request           []byte
		shouldReply       bool
		apiKey            int16
		shouldSkipRequest bool
		extraCheck        func(t *testing.T, handlerCalledTimes int)
		expectedLoggedErr string
		sleepBeforeCheck  time.Duration
		handler           watermillmessage.HandlerFunc
	}{
		{
			name:        "Handler success",
			request:     generateProduceRequestV8("valid message"),
			shouldReply: true,
			handler:     noopHandler,
		},
		{
			name:              "Handler error means Nack. Message is resent infinitely.",
			request:           generateProduceRequestV8("something is gonna make it fail!"),
			shouldReply:       true,
			expectedLoggedErr: "message is invalid, meaning Nack will be sent and message will be resent infinitely",
			extraCheck: func(t *testing.T, handlerCalls int) {
				assert.Greater(t, handlerCalls, 1)
			},
			handler: func(msg *watermillmessage.Message) ([]*watermillmessage.Message, error) {
				return nil, errors.New("message is invalid, meaning Nack will be sent and message will be resent infinitely") // This will send a Nack, meaning message will be resent.
			},
			sleepBeforeCheck: time.Millisecond, // letting the handlers be called several times due to Nack produced by returning an error.
		},
		{
			name:              "Other Requests (different than Produce type) are skipped",
			request:           []byte{0, 0, 0, 1}, // fake payload that should not be read.
			apiKey:            int16(42),
			shouldReply:       true,
			shouldSkipRequest: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			log := logrustest.NewGlobal()

			r, err := watermillmessage.NewRouter(watermillmessage.RouterConfig{}, message.NewWatermillLogrusLogger(logrus.StandardLogger()))
			require.NoError(t, err)

			var handlerCalls uint32
			// Wrapping handler so we can add a spy.
			handler := func(msg *watermillmessage.Message) ([]*watermillmessage.Message, error) {
				atomic.AddUint32(&handlerCalls, 1)
				if test.handler != nil {
					return test.handler(msg)
				}

				return noopHandler(msg)
			}
			h := NewProduceRequestHandler(r, handler)

			go func() {
				// Running the router
				require.NoError(t, r.Run(context.Background()))
			}()
			t.Cleanup(func() {
				// Closing the router
				require.NoError(t, r.Close())
			})

			// Do not start test until the router is fully up and running.
			<-r.Running()

			kv := &kafkaprotocol.RequestKeyVersion{
				ApiVersion: 8,                            // All test data was grabbed from a Produce Request version 8.
				ApiKey:     test.apiKey,                  // default is 0, which is a Produce Request
				Length:     int32(len(test.request) + 4), // 4 bytes are ApiKey + Version located in all request headers (already read by the time of validating the msg).
			}
			readBytes := bytes.NewBuffer(nil)
			shouldReply, err := h.Handle(kv, bytes.NewReader(test.request), &kafkaproxy.RequestsLoopContext{}, readBytes)
			assert.NoError(t, err)
			assert.Equal(t, test.shouldReply, shouldReply)

			if test.shouldSkipRequest {
				assert.Empty(t, readBytes.Bytes()) // Payload is never read.
			} else {
				assert.Equal(t, readBytes.Len(), len(test.request))
			}

			if test.sleepBeforeCheck > 0 {
				time.Sleep(test.sleepBeforeCheck)
			}

			if test.extraCheck != nil {
				test.extraCheck(t, int(atomic.LoadUint32(&handlerCalls)))
			}

			if test.expectedLoggedErr != "" {
				entry := log.LastEntry()
				require.Contains(t, entry.Data, logrus.ErrorKey)
				assert.EqualError(t, entry.Data[logrus.ErrorKey].(error), test.expectedLoggedErr)
			} else {
				for _, l := range log.AllEntries() {
					assert.NotEqualf(t, l.Level, logrus.ErrorLevel, "%q logged error unexpected", l.Message) // We don't have a notification mechanism for errors yet
				}
			}
		})
	}
}

func noopHandler(msg *watermillmessage.Message) ([]*watermillmessage.Message, error) {
	return []*watermillmessage.Message{msg}, nil
}

func generateProduceRequestV8(payload string) []byte {
	// Note: Taking V8 as random version.
	buf := bytes.NewBuffer(nil)

	// Request headers
	//
	// correlation_id: 0, 0, 0, 6
	// client_id_size: 0, 16
	// client_id: 99, 111, 110, 115, 111, 108, 101, 45, 112, 114, 111, 100, 117, 99, 101, 114
	// transactional_id_size: 255, 255
	// acks: 0, 1
	buf.Write([]byte{0, 0, 0, 6, 0, 16, 99, 111, 110, 115, 111, 108, 101, 45, 112, 114, 111, 100, 117, 99, 101, 114, 255, 255, 0, 1})

	// timeout: 0, 0, 5, 200
	// topics count: 0, 0, 0, 1
	// topic name string len: 0, 4
	// topic name: 100, 101, 109, 111
	// partition count: 0, 0, 0, 1
	// partition: 0, 0, 0, 0
	buf.Write([]byte{0, 0, 5, 220, 0, 0, 0, 1, 0, 4, 100, 101, 109, 111, 0, 0, 0, 1, 0, 0, 0, 0})

	// request size <int32>
	requestSize := make([]byte, 4)
	requestSizeInt := uint32(68 + len(payload))
	binary.BigEndian.PutUint32(requestSize, requestSizeInt)
	buf.Write(requestSize)

	// base offset: 0, 0, 0, 0, 0, 0, 0, 0
	baseOffset := []byte{0, 0, 0, 0, 0, 0, 0, 0}
	buf.Write(baseOffset)

	// batch len <int32>
	batchLen := make([]byte, 4)
	binary.BigEndian.PutUint32(batchLen, requestSizeInt-uint32(len(baseOffset)+len(batchLen)))
	buf.Write(batchLen)

	//   partition leader epoch: 255, 255, 255, 255
	//   version: 2
	leaderEpochAndVersion := []byte{255, 255, 255, 255, 2}
	buf.Write(leaderEpochAndVersion)

	// CRC32 <int32>
	crc32ReservationStart := buf.Len()
	buf.Write([]byte{0, 0, 0, 0}) // reserving int32 for crc calculation once the rest of the request is generated.

	//   attributes: 0, 0
	//   last offset delta: 0, 0, 0, 0
	//   first timestamp: 0, 0, 1, 122, 129, 58, 129, 47
	//   max timestamp: 0, 0, 1, 122, 129, 58, 129, 47
	//   producer id: 255, 255, 255, 255, 255, 255, 255, 255
	//   producer epoc: 255, 255
	//   base sequence: 255, 255, 255, 255
	//   records https://kafka.apache.org/documentation/#record
	//     amount: 0, 0, 0, 1
	buf.Write([]byte{0, 0, 0, 0, 0, 0, 0, 0, 1, 122, 129, 58, 129, 47, 0, 0, 1, 122, 129, 58, 129, 47, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 0, 0, 0, 1})

	// record len <int8>
	// attributes + timestamp delta + offset + key + payload len field + actual payload len
	recordLen := make([]byte, 1)
	binary.PutVarint(recordLen, int64(4+1+len(payload)+1))
	buf.Write(recordLen)

	//       attributes: 0
	//       timestamp delta: 0
	//       offset delta: 0
	//       key: 1
	buf.Write([]byte{0, 0, 0, 1})

	// message payload len
	payloadLen := make([]byte, 1)
	binary.PutVarint(payloadLen, int64(len(payload)))
	buf.Write(payloadLen)

	// Payload
	buf.Write([]byte(payload))

	// Headers: 0
	buf.WriteByte(0)

	table := crc32.MakeTable(crc32.Castagnoli)
	crc32Calculator := crc32.New(table)
	_, _ = crc32Calculator.Write(buf.Bytes()[crc32ReservationStart+4:])

	hash := crc32Calculator.Sum(make([]byte, 0))
	for i := 0; i < len(hash); i++ {
		buf.Bytes()[crc32ReservationStart+i] = hash[i]
	}

	return buf.Bytes()
}
