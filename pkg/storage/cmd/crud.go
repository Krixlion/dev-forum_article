package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/krixlion/dev-forum_article/pkg/entity"
	"github.com/krixlion/dev-forum_article/pkg/event"
	"github.com/krixlion/dev-forum_article/pkg/tracing"

	"github.com/EventStore/EventStore-Client-Go/v3/esdb"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

func (db DB) Close() error {
	return db.client.Close()
}

func (db DB) Create(ctx context.Context, article entity.Article) error {
	ctx, span := otel.Tracer(tracing.ServiceName).Start(ctx, "Create")
	defer span.End()

	jsonArticle, err := json.Marshal(article)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	e := event.Event{
		Entity:    entity.ArticleEntity,
		Type:      event.Created,
		Body:      jsonArticle,
		Timestamp: time.Now(),
	}

	data, err := json.Marshal(e)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	eventData := esdb.EventData{
		ContentType: esdb.ContentTypeJson,
		EventType:   string(e.Type),
		Data:        data,
	}
	streamID := fmt.Sprintf("%s-%s", entity.ArticleEntity, article.Id)

	_, err = db.client.AppendToStream(ctx, streamID, esdb.AppendToStreamOptions{}, eventData)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	return nil
}

func (db DB) Update(ctx context.Context, article entity.Article) error {
	ctx, span := otel.Tracer(tracing.ServiceName).Start(ctx, "Update")
	defer span.End()

	jsonArticle, err := json.Marshal(article)
	if err != nil {
		return err
	}

	e := event.Event{
		Entity:    entity.ArticleEntity,
		Type:      event.Updated,
		Body:      jsonArticle,
		Timestamp: time.Now(),
	}

	data, err := json.Marshal(e)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	lastEvent, err := db.lastRevision(ctx, article.Id)
	if err != nil {
		return err
	}

	appendOpts := esdb.AppendToStreamOptions{
		ExpectedRevision: esdb.Revision(lastEvent.OriginalEvent().EventNumber),
	}

	eventData := esdb.EventData{
		ContentType: esdb.ContentTypeJson,
		EventType:   string(e.Type),
		Data:        data,
	}
	streamID := fmt.Sprintf("%s-%s", entity.ArticleEntity, article.Id)

	_, err = db.client.AppendToStream(ctx, streamID, appendOpts, eventData)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	return nil
}

func (db DB) Delete(ctx context.Context, id string) error {
	ctx, span := otel.Tracer(tracing.ServiceName).Start(ctx, "Delete")
	defer span.End()

	jsonID, err := json.Marshal(id)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	e := event.Event{
		Entity:    entity.ArticleEntity,
		Type:      event.Deleted,
		Body:      jsonID,
		Timestamp: time.Now(),
	}

	data, err := json.Marshal(e)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	eventData := esdb.EventData{
		ContentType: esdb.ContentTypeJson,
		EventType:   string(e.Type),
		Data:        data,
	}
	streamID := fmt.Sprintf("%s-%s", entity.ArticleEntity, id)

	_, err = db.client.AppendToStream(ctx, streamID, esdb.AppendToStreamOptions{}, eventData)

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	return nil
}

func (db DB) lastRevision(ctx context.Context, articleId string) (*esdb.ResolvedEvent, error) {
	ctx, span := otel.Tracer(tracing.ServiceName).Start(ctx, "lastRevision")
	defer span.End()

	readOpts := esdb.ReadStreamOptions{
		Direction: esdb.Backwards,
		From:      esdb.End{},
	}

	streamID := fmt.Sprintf("%s-%s", entity.ArticleEntity, articleId)

	stream, err := db.client.ReadStream(ctx, streamID, readOpts, 1)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	defer stream.Close()

	lastEvent, err := stream.Recv()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	return lastEvent, nil
}
