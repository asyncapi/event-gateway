package config

import (
	"strings"

	"github.com/asyncapi/event-gateway/kafka"
)

// App holds the config for the whole application.
type App struct {
	Debug        bool        `desc:"Enable or disable debug logs"`
	AsyncAPIDoc  []byte      `split_words:"true" desc:"Path or URL to a valid AsyncAPI doc (v2.0.0 is only supported)"`
	WSServerPort int         `split_words:"true" default:"5000" desc:"Port for the Websocket server. Used for debugging events"`
	KafkaProxy   *KafkaProxy `split_words:"true"`
}

// Opt is a functional option used for configuring an App.
type Opt func(*App)

// NewApp creates a App config with defaults.
func NewApp(opts ...Opt) *App {
	c := &App{KafkaProxy: NewKafkaProxy()}
	for _, opt := range opts {
		opt(c)
	}

	return c
}

// ProxyConfig creates a config struct for the Kafka Proxy.
func (c App) ProxyConfig() (*kafka.ProxyConfig, error) {
	return c.KafkaProxy.ProxyConfig(c.AsyncAPIDoc, c.Debug)
}

type pipeSeparatedValues struct {
	Values []string
}

func (b *pipeSeparatedValues) Set(value string) error {
	b.Values = strings.Split(value, "|")
	return nil
}
