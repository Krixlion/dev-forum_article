package cmd

import (
	"fmt"

	"github.com/EventStore/EventStore-Client-Go/v3/esdb"
	"github.com/krixlion/dev-forum_article/pkg/event"
)

type DB struct {
	client *esdb.Client
	eh     event.Handler
	url    string
}

func formatConnString(port, host, user, pass string) string {
	return fmt.Sprintf("esdb://%s:%s@%s:%s?tls=false", user, pass, host, port)
}

func MakeDB(port, host, user, pass string) DB {
	url := formatConnString(port, host, user, pass)
	settings, err := esdb.ParseConnectionString(url)

	if err != nil {
		panic(err)
	}

	client, _ := esdb.NewClient(settings)

	return DB{
		url:    url,
		client: client,
		// eh:     mq.MakeSession(),
	}
}
