package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"mime"
	"net/http"

	"github.com/google/go-github/v47/github"
	"github.com/google/uuid"
	"github.com/pentops/registry/gen/o5/registry/github/v1/github_pb"
	"gopkg.daemonl.com/log"

	"github.com/aws/aws-lambda-go/events"
	"github.com/pentops/o5-runtime-sidecar/awsmsg"
)

type WebhookWorker struct {
	publishers  []awsmsg.Publisher
	secretToken []byte
}

func NewWebhookWorker(secretToken string, publishers ...awsmsg.Publisher) (*WebhookWorker, error) {
	return &WebhookWorker{
		secretToken: []byte(secretToken),
		publishers:  publishers,
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

	anyEvent, err := github.ParseWebHook(header.Get("X-GitHub-Event"), verifiedPayload)
	if err != nil {
		return nil, fmt.Errorf("parsing webhook: %w", err)
	}

	event, ok := anyEvent.(*github.PushEvent)
	if !ok {
		return &events.APIGatewayV2HTTPResponse{
			StatusCode: http.StatusBadRequest,
			Body:       fmt.Sprintf("webhooks should only be configured for push events, got %T", anyEvent),
		}, nil
	}

	if err := validatePushEvent(event); err != nil {
		return &events.APIGatewayV2HTTPResponse{
			StatusCode: http.StatusBadRequest,
			Body:       err.Error(),
		}, nil
	}

	if *event.After == emptyCommit {
		return &events.APIGatewayV2HTTPResponse{
			StatusCode: http.StatusBadRequest,
			Body:       "push event has empty after commit",
			// TODO: This might be what happens on delete?
		}, nil
	}

	pushID := uuid.NewSHA1(pushNamespace, []byte(fmt.Sprintf("%d", *event.PushID))).String()

	// Send Message to SNS
	msg := &github_pb.PushMessage{
		Before: *event.Before,
		After:  *event.After,
		Ref:    *event.Ref,
		Repo:   *event.Repo.Name,
		Owner:  *event.Repo.Owner.Name,
	}

	wireMessage, err := msg.O5Message(pushID)
	if err != nil {
		return nil, err
	}

	for _, publisher := range ww.publishers {
		if err := publisher.Publish(ctx, wireMessage); err != nil {
			return nil, err
		}
	}

	return &events.APIGatewayV2HTTPResponse{
		StatusCode: http.StatusOK,
		Body:       "ok",
	}, nil

}

var pushNamespace = uuid.MustParse("B15B01C2-0228-49E7-8432-EA17E5A1B69C")

const emptyCommit = "0000000000000000000000000000000000000000"

func validatePushEvent(event *github.PushEvent) error {

	if event.Ref == nil {
		return fmt.Errorf("nil 'ref' on push event")
	}

	if event.Repo == nil {
		return fmt.Errorf("nil 'repo' on push event")
	}

	if event.Repo.Owner == nil {
		return fmt.Errorf("nil 'repo.owner' on push event")
	}

	if event.Repo.Owner.Name == nil {
		return fmt.Errorf("nil 'repo.owner.name' on push event")
	}

	if event.Repo.Name == nil {
		return fmt.Errorf("nil 'repo.name' on push event")
	}

	if event.After == nil {
		return fmt.Errorf("nil 'after' on push event")
	}

	if event.Before == nil {
		return fmt.Errorf("nil 'before' on push event")
	}
	return nil

}
