package kafka

import (
	"bytes"
	"context"
	"io"
	"strings"

	"github.com/Shopify/sarama"
	"github.com/asyncapi/event-gateway/proxy"
	server "github.com/grepplabs/kafka-proxy/cmd/kafka-proxy"
	kafkaproxy "github.com/grepplabs/kafka-proxy/proxy"
	kafkaprotocol "github.com/grepplabs/kafka-proxy/proxy/protocol"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// Kafka request API Keys. See https://kafka.apache.org/protocol#protocol_api_keys.
const (
	// RequestAPIKeyProduce is the Kafka request API Key for the Produce Request.
	RequestAPIKeyProduce = 0
)

// Context is the context that surrounds a Message.
type Context struct {
	Topic string
}

// Message is a message flowing through a Kafka topic.
type Message struct {
	Context Context
	Key     []byte
	Value   []byte
	Headers []sarama.RecordHeader
}

// MessageHandler handles a Kafka message.
// If error is returned, kafka request will fail.
// Note: Message manipulation is not allowed.
type MessageHandler func(Message) error

// NewProxy creates a new Kafka Proxy based on a given configuration.
func NewProxy(c *ProxyConfig) (proxy.Proxy, error) {
	if c == nil {
		return nil, errors.New("config should be provided")
	}

	if err := c.Validate(); err != nil {
		return nil, err
	}

	// Yeah, not a good practice at all but I guess it's fine for now.
	kafkaproxy.ActualDefaultRequestHandler.RequestKeyHandlers.Set(RequestAPIKeyProduce, NewProduceRequestHandler(c.MessageHandlers...))

	// Setting some defaults.
	_ = server.Server.Flags().Set("default-listener-ip", "0.0.0.0") // Binding to all local network interfaces. Needed for external calls.

	if c.BrokersMapping == nil {
		return nil, errors.New("Brokers mapping is required")
	}

	if c.Debug {
		_ = server.Server.Flags().Set("log-level", "debug")
	}

	for _, v := range c.ExtraConfig {
		f := strings.Split(v, "=")
		_ = server.Server.Flags().Set(f[0], f[1])
	}

	for _, v := range c.BrokersMapping {
		_ = server.Server.Flags().Set("bootstrap-server-mapping", v)
	}

	for _, v := range c.DialAddressMapping {
		_ = server.Server.Flags().Set("dial-address-mapping", v)
	}

	return func(_ context.Context) error {
		return server.Server.Execute()
	}, nil
}

// NewProduceRequestHandler creates a new request key handler for the Produce Request.
func NewProduceRequestHandler(msgHandlers ...MessageHandler) kafkaproxy.KeyHandler {
	return &produceRequestHandler{
		msgHandlers: msgHandlers,
	}
}

type produceRequestHandler struct {
	msgHandlers []MessageHandler
}

func (h *produceRequestHandler) Handle(requestKeyVersion *kafkaprotocol.RequestKeyVersion, src io.Reader, ctx *kafkaproxy.RequestsLoopContext, bufferRead *bytes.Buffer) (shouldReply bool, err error) {
	if len(h.msgHandlers) == 0 {
		logrus.Infoln("No message handlers were set. Skipping produceRequestHandler")
		return true, nil
	}

	if requestKeyVersion.ApiKey != RequestAPIKeyProduce {
		return true, nil
	}

	// TODO error handling should be responsibility of an error handler instead of being just logged.
	shouldReply, err = kafkaproxy.DefaultProduceKeyHandlerFunc(requestKeyVersion, src, ctx, bufferRead)
	if err != nil {
		return
	}

	msg := make([]byte, int64(requestKeyVersion.Length-int32(4+bufferRead.Len())))
	if _, err = io.ReadFull(io.TeeReader(src, bufferRead), msg); err != nil {
		return
	}

	// Hack for making compatible greplabs/kafka-proxy processor with Shopify/sarama ProduceRequest.
	// As both Transactional ID and ACKs has been read already by the processor, we fake them here because the Sarama decoder expects them to be present.
	// This information is not going to be used later on, as this is a read-only message.
	// transactional_id_size: 255, 255 | acks: 0, 1
	// TODO is there a way this info can be subtracted from kafka-proxy?
	msg = append([]byte{255, 255, 0, 1}, msg...)

	var req sarama.ProduceRequest
	if err = sarama.DoVersionedDecode(msg, &req, requestKeyVersion.ApiVersion); err != nil {
		logrus.WithError(err).Error("error decoding ProduceRequest")
		return shouldReply, nil
	}

	msgs := h.extractMessages(req)
	if len(msgs) == 0 {
		logrus.Error("The produce request has no messages")
		return
	}

	for _, m := range msgs {
		for _, h := range h.msgHandlers {
			if err := h(m); err != nil {
				logrus.WithError(err).Error("error handling message")
				return shouldReply, nil
			}
		}
	}

	return shouldReply, nil
}

func (h *produceRequestHandler) extractMessages(req sarama.ProduceRequest) []Message {
	var msgs []Message

	for topic, r := range req.Records {
		for _, s := range r {
			if s.RecordBatch != nil {
				for _, r := range s.RecordBatch.Records {
					// Fixing indirection here
					headers := make([]sarama.RecordHeader, len(r.Headers))
					for i := 0; i < len(r.Headers); i++ {
						headers[i] = *r.Headers[i]
					}

					msgs = append(msgs, Message{
						Context: Context{
							Topic: topic,
						},
						Key:     r.Key,
						Value:   r.Value,
						Headers: headers,
					})
				}
			}
			if s.MsgSet != nil {
				for _, mb := range s.MsgSet.Messages {
					msgs = append(msgs, Message{
						Context: Context{
							Topic: topic,
						},
						Key:   mb.Msg.Key,
						Value: mb.Msg.Value,
					})
				}
			}
		}
	}
	return msgs
}
