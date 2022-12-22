package rabbitmq

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/krixlion/dev-forum_article/pkg/logging"
	"github.com/krixlion/dev-forum_article/pkg/tracing"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/sony/gobreaker"
	"golang.org/x/sync/errgroup"
)

const (
	connectionError = 1
	channelError    = 2
)

type RabbitMQ struct {
	ConsumerName    string
	ctx             context.Context
	shutdown        context.CancelFunc
	config          Config
	connection      *amqp.Connection
	mutex           sync.RWMutex            // Mutex protecting connection during reconnecting.
	url             string                  // Connection string to RabbitMQ broker.
	notifyConnClose chan *amqp.Error        // Channel to watch for errors from the broker in order to renew the connection.
	publishQueue    chan Message            // Queue for messages waiting to be republished.
	readChannel     chan chan *amqp.Channel // Access channel for accessing the RabbitMQ Channel in a thread-safe way.
	breaker         *gobreaker.TwoStepCircuitBreaker
	logger          logging.Logger
}

// NewRabbitMQ returns a new connection struct.
// Run() method needs to be invoked before any operations on this struct are performed.
//
//	func Example() {
//		user := "guest"
//		pass := "guest"
//		host := "localhost"
//		port := "5672"
//		consumer := "user-service" //  Unique name for each consumer used to sign messages.
//
//		// You can use rabbitmq.DefaultConfig() instead.
//		config := Config{
//			QueueSize:         100,             // Max number of messages internally queued for publishing.
//			ReconnectInterval: time.Second * 2, // Time between reconnect attempts.
//			MaxRequests:       5,               // Number of requests allowed to half-open state.
//			ClearInterval:     time.Second * 5, // Time after which failed calls count is cleared.
//			ClosedTimeout:     time.Second * 5, // Time after which closed state becomes half-open.
//		}
//
//		rabbit := rabbitmq.NewRabbitMQ(consumer, user, pass, host, port, config)
//		rabbit.Run()
//		defer rabbit.Close()
//	}
func NewRabbitMQ(consumer, user, pass, host, port string, config Config) *RabbitMQ {

	url := fmt.Sprintf("amqp://%s:%s@%s:%s/", user, pass, host, port)

	ctx, cancel := context.WithCancel(context.Background())
	logger, _ := logging.NewLogger()

	return &RabbitMQ{
		publishQueue:    make(chan Message, config.QueueSize),
		readChannel:     make(chan chan *amqp.Channel),
		notifyConnClose: make(chan *amqp.Error, 4),
		ctx:             ctx,
		shutdown:        cancel,
		logger:          logger,
		url:             url,
		ConsumerName:    consumer,
		config:          config,
		breaker: gobreaker.NewTwoStepCircuitBreaker(gobreaker.Settings{
			Name:        consumer,
			MaxRequests: config.MaxRequests,
			Interval:    config.ClearInterval,
			Timeout:     config.ClosedTimeout,
		}),
	}
}

// Run initializes the RabbitMQ connection and starts the system in
// seperate goroutines while blocking the goroutine it was called from.
//
// Do not forget to Close() in order to shutdown the system.
func (mq *RabbitMQ) Run() error {
	ctx, span := otel.Tracer(tracing.ServiceName).Start(mq.ctx, "rabbitmq.Run")
	defer span.End()

	if err := mq.dial(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	errg, ctx := errgroup.WithContext(ctx)

	mq.runPublishQueue(ctx)

	errg.Go(func() error {
		return mq.handleConnectionErrors(ctx)
	})

	errg.Go(func() error {
		return mq.handleChannelReads(ctx)
	})

	if err := errg.Wait(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	return nil
}

// Close active connection gracefully.
func (mq *RabbitMQ) Close() error {
	defer mq.shutdown()
	if mq.connection != nil && !mq.connection.IsClosed() {
		mq.logger.Log(mq.ctx, "Closing active connections")
		if err := mq.connection.Close(); err != nil {
			mq.logger.Log(mq.ctx, "Failed to close active connections", "err", err)
			return err
		}
	}
	return nil
}

func (mq *RabbitMQ) runPublishQueue(ctx context.Context) {
	ctx, span := otel.Tracer(tracing.ServiceName).Start(ctx, "rabbitmq.runPublishQueue")
	defer span.End()

	preparedMessages := mq.prepareExchangePipelined(ctx, mq.publishQueue)
	mq.publishPipelined(ctx, preparedMessages)
}

// handleChannelReads is meant to be run in a seperate goroutine.
func (mq *RabbitMQ) handleChannelReads(ctx context.Context) error {
	ctx, span := otel.Tracer(tracing.ServiceName).Start(ctx, "rabbitmq.handleChannelReads")
	defer span.End()

	for {
		select {
		case req := <-mq.readChannel:
			succedeed, err := mq.breaker.Allow()
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
				req <- nil
			}

			channel, err := mq.connection.Channel()
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
				req <- nil
				mq.logger.Log(ctx, "Failed opening a new channel", "err", err)
				succedeed(false)
				continue
			}

			succedeed(true)
			req <- channel

		case <-ctx.Done():
			mq.logger.Log(ctx, "Stopped listening to channel reads")
			return ctx.Err()
		}
	}
}

// handleConnectionErrors is meant to be run in a seperate goroutine.
func (mq *RabbitMQ) handleConnectionErrors(ctx context.Context) error {
	ctx, span := otel.Tracer(tracing.ServiceName).Start(ctx, "rabbitmq.handleConnectionErrors")
	defer span.End()

	for {
		select {
		case e := <-mq.notifyConnClose:

			if e == nil {
				continue
			}

			for {
				err := mq.dial()
				if err == nil {
					break
				}

				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())

				mq.logger.Log(ctx, "Reconnecting to RabbitMQ")
				time.Sleep(mq.config.ReconnectInterval)
			}

		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// dial renews current TCP connection.
func (mq *RabbitMQ) dial() error {
	callSucceded, err := mq.breaker.Allow()
	if err != nil {
		return err
	}

	conn, err := amqp.Dial(mq.url)
	if err != nil {
		if isConnectionError(err.(*amqp.Error)) {
			callSucceded(false)
		}
		// Error did not render broker unavailable.
		callSucceded(true)

		return err
	}
	callSucceded(true)

	mq.mutex.Lock()
	defer mq.mutex.Unlock()
	mq.connection = conn

	return nil
}

// channel returns a channel in a thread-safe way.
func (mq *RabbitMQ) channel() *amqp.Channel {
	for {
		ask := make(chan *amqp.Channel)
		mq.readChannel <- ask

		channel := <-ask
		if channel != nil {
			return channel
		}

		time.Sleep(mq.config.ReconnectInterval)
	}
}

func errorType(code int) int {
	switch code {
	case
		amqp.ContentTooLarge,    // 311
		amqp.NoConsumers,        // 313
		amqp.AccessRefused,      // 403
		amqp.NotFound,           // 404
		amqp.ResourceLocked,     // 405
		amqp.PreconditionFailed: // 406
		return channelError

	case
		amqp.ConnectionForced, // 320
		amqp.InvalidPath,      // 402
		amqp.FrameError,       // 501
		amqp.SyntaxError,      // 502
		amqp.CommandInvalid,   // 503
		amqp.ChannelError,     // 504
		amqp.UnexpectedFrame,  // 505
		amqp.ResourceError,    // 506
		amqp.NotAllowed,       // 530
		amqp.NotImplemented,   // 540
		amqp.InternalError:    // 541
		fallthrough

	default:
		return connectionError
	}
}

func isConnectionError(err *amqp.Error) bool {
	return errorType(err.Code) == connectionError
}
