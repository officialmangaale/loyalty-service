package orderevents

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

type EventProcessor interface {
	Process(context.Context, Envelope, []byte) (ProcessResult, error)
	ReplayHeld(context.Context, int) error
}

type SQSAPI interface {
	ReceiveMessage(context.Context, *sqs.ReceiveMessageInput, ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(context.Context, *sqs.DeleteMessageInput, ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
}

type SQSConsumer struct {
	client    SQSAPI
	queueURL  string
	processor EventProcessor
	logger    *slog.Logger
}

func NewSQSConsumer(client SQSAPI, queueURL string, processor EventProcessor, logger *slog.Logger) (*SQSConsumer, error) {
	queueURL = strings.TrimSpace(queueURL)
	if client == nil || queueURL == "" || processor == nil || logger == nil {
		return nil, errors.New("order event consumer requires SQS, queue URL, processor, and logger")
	}
	return &SQSConsumer{client: client, queueURL: queueURL, processor: processor, logger: logger}, nil
}

func (c *SQSConsumer) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := c.processor.ReplayHeld(ctx, 25); err != nil && !errors.Is(err, context.Canceled) {
			c.logger.Error("held order event replay failed", "error_code", "held_replay_failed")
		}
		output, err := c.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(c.queueURL),
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     10,
		})
		if err != nil {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				return
			}
			c.logger.Error("receive order events failed", "error_code", "sqs_receive_failed")
			if !waitForRetry(ctx) {
				return
			}
			continue
		}
		for _, message := range output.Messages {
			payload, event, err := decodeSQSBody(aws.ToString(message.Body))
			if err != nil {
				c.logger.Warn("invalid order event left for queue redrive", "message_id", aws.ToString(message.MessageId), "error_code", "invalid_event")
				continue
			}
			if _, err := c.processor.Process(ctx, event, payload); err != nil {
				c.logger.Warn("order event processing failed", "event_id", event.EventID.String(), "error_code", "event_processing_failed")
				continue
			}
			if strings.TrimSpace(aws.ToString(message.ReceiptHandle)) == "" {
				continue
			}
			if _, err := c.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
				QueueUrl: c.queueURLPointer(), ReceiptHandle: message.ReceiptHandle,
			}); err != nil {
				c.logger.Warn("processed order event delete failed", "event_id", event.EventID.String(), "error_code", "sqs_delete_failed")
			}
		}
	}
}

func decodeSQSBody(body string) ([]byte, Envelope, error) {
	direct := []byte(strings.TrimSpace(body))
	if event, err := DecodeStrict(direct); err == nil {
		return direct, event, nil
	}
	var notification struct {
		Message string `json:"Message"`
	}
	if err := json.Unmarshal(direct, &notification); err != nil || strings.TrimSpace(notification.Message) == "" {
		return nil, Envelope{}, ErrInvalidEvent
	}
	payload := []byte(notification.Message)
	event, err := DecodeStrict(payload)
	if err != nil {
		return nil, Envelope{}, err
	}
	return payload, event, nil
}

func (c *SQSConsumer) queueURLPointer() *string { return aws.String(c.queueURL) }

func waitForRetry(ctx context.Context) bool {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
