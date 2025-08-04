package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"mime"
	"net/http"
	"strings"

	"github.com/google/go-github/v47/github"
	"github.com/pentops/log.go/log"

	"github.com/aws/aws-lambda-go/events"
	"github.com/pentops/o5-messaging/gen/o5/messaging/v1/messaging_pb"
	"github.com/pentops/o5-messaging/gen/o5/messaging/v1/messaging_tpb"
	"github.com/pentops/o5-messaging/o5msg"
)

type WebhookWorker struct {
	publishers  []Publisher
	secretToken []byte

	Source SourceConfig
}

type SourceConfig struct {
	SourceApp string
	SourceEnv string
}

type Publisher interface {
	Publish(ctx context.Context, message *messaging_pb.Message) error
	PublisherID() string
}

func NewWebhookWorker(secretToken string, source SourceConfig, publishers ...Publisher) (*WebhookWorker, error) {
	return &WebhookWorker{
		secretToken: []byte(secretToken),
		publishers:  publishers,
		Source:      source,
	}, nil
}

func (ww *WebhookWorker) HandleLambda(ctx context.Context, request *events.APIGatewayV2HTTPRequest) (*events.APIGatewayV2HTTPResponse, error) {
	header := &http.Header{}
	for k, v := range request.Headers {
		header.Add(k, v)
	}

	signature := header.Get(github.SHA256SignatureHeader)
	if signature == "" {
		signature = header.Get(github.SHA1SignatureHeader)
	}

	deliveryID := header.Get(github.DeliveryIDHeader)
	if deliveryID == "" {
		return &events.APIGatewayV2HTTPResponse{
			StatusCode: http.StatusBadRequest,
			Body:       "missing delivery ID",
		}, nil
	}

	contentType, _, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil {
		return nil, fmt.Errorf("parse media type from '%s': %w", header.Get("Content-Type"), err)
	}

	bodyBytes := []byte(request.Body)
	if request.IsBase64Encoded {
		bodyBytes, err = base64.StdEncoding.DecodeString(request.Body)
		if err != nil {
			return nil, fmt.Errorf("decoding body: %w", err)
		}
	}

	log.WithField(ctx, "body", string(bodyBytes)).Debug("Received body")

	bodyReader := bytes.NewReader(bodyBytes)

	verifiedPayload, err := github.ValidatePayloadFromBody(contentType, bodyReader, signature, ww.secretToken)
	if err != nil {
		return nil, fmt.Errorf("validating payload: %w", err)
	}

	msg := &messaging_tpb.RawMessage{
		Topic:   fmt.Sprintf("github:%s", header.Get("X-GitHub-Event")),
		Payload: verifiedPayload,
	}

	return ww.publish(ctx, msg)
}

func (ww *WebhookWorker) publish(ctx context.Context, msg *messaging_tpb.RawMessage) (*events.APIGatewayV2HTTPResponse, error) {

	wireMessage, err := o5msg.WrapMessage(msg)
	if err != nil {
		return nil, err
	}

	wireMessage.SourceApp = ww.Source.SourceApp
	wireMessage.SourceEnv = ww.Source.SourceEnv
	wireMessage.DestinationTopic = msg.Topic

	output := make([]string, 0, len(ww.publishers))
	output = append(output, fmt.Sprintf("O5 Message ID: %s", wireMessage.MessageId))
	for _, publisher := range ww.publishers {
		if err := publisher.Publish(ctx, wireMessage); err != nil {
			return nil, err
		}
		output = append(output, fmt.Sprintf("Published to %s", publisher.PublisherID()))
	}

	return &events.APIGatewayV2HTTPResponse{
		StatusCode: http.StatusOK,
		Body:       fmt.Sprintf("HOOK OK\n%s", strings.Join(output, "\n")),
	}, nil

}
