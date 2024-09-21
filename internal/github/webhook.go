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
	"github.com/pentops/o5-messaging/o5msg"
	"github.com/pentops/o5-runtime-sidecar/awsmsg"
	"github.com/pentops/registry/gen/j5/registry/github/v1/github_tpb"
)

type WebhookWorker struct {
	publishers  []awsmsg.Publisher
	secretToken []byte

	Source SourceConfig
}

type SourceConfig struct {
	SourceApp string
	SourceEnv string
}

func NewWebhookWorker(secretToken string, source SourceConfig, publishers ...awsmsg.Publisher) (*WebhookWorker, error) {
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

	anyEvent, err := github.ParseWebHook(header.Get("X-GitHub-Event"), verifiedPayload)
	if err != nil {
		return nil, fmt.Errorf("parsing webhook: %w", err)
	}

	switch event := anyEvent.(type) {
	case *github.PushEvent:
		return ww.handlePushEvent(ctx, deliveryID, event)

	case *github.CheckRunEvent:
		return ww.handleCheckRunEvent(ctx, deliveryID, event)

	default:
		return &events.APIGatewayV2HTTPResponse{
			StatusCode: http.StatusBadRequest,
			Body:       fmt.Sprintf("unhandled event type %s", anyEvent),
		}, nil

	}

}

func (ww *WebhookWorker) handleCheckRunEvent(ctx context.Context, deliveryID string, event *github.CheckRunEvent) (*events.APIGatewayV2HTTPResponse, error) {
	if err := validateCheckRunEvent(event); err != nil {
		return &events.APIGatewayV2HTTPResponse{
			StatusCode: http.StatusBadRequest,
			Body:       err.Error(),
		}, nil
	}

	msg := &github_tpb.CheckRunMessage{
		DeliveryId:   deliveryID,
		Action:       *event.Action,
		Owner:        *event.Repo.Owner.Login,
		Repo:         *event.Repo.Name,
		CheckRunName: *event.CheckRun.Name,
		CheckRunId:   *event.CheckRun.ID,
		Ref:          fmt.Sprintf("refs/heads/%s", *event.CheckRun.CheckSuite.HeadBranch),
		Before:       *event.CheckRun.CheckSuite.BeforeSHA,
		After:        *event.CheckRun.CheckSuite.AfterSHA,
	}

	return ww.publish(ctx, msg)
}

func validateCheckRunEvent(event *github.CheckRunEvent) error {
	if event.Action == nil {
		return fmt.Errorf("nil 'action' on check_run event")
	}
	if err := validateRepo(event.Repo); err != nil {
		return err
	}
	if event.CheckRun == nil {
		return fmt.Errorf("nil 'check_run' on check_run event")
	}
	if event.CheckRun.Name == nil {
		return fmt.Errorf("nil 'check_run.name' on check_run event")
	}
	if event.CheckRun.ID == nil {
		return fmt.Errorf("nil 'check_run.id' on check_run event")
	}

	if event.CheckRun.CheckSuite == nil {
		return fmt.Errorf("nil 'check_run.check_suite' on check_run event")
	}
	if event.CheckRun.CheckSuite.HeadBranch == nil {
		return fmt.Errorf("nil 'check_run.check_suite.head_branch' on check_run event")
	}
	if event.CheckRun.CheckSuite.BeforeSHA == nil {
		return fmt.Errorf("nil 'check_run.check_suite.before_sha' on check_run event")
	}
	if event.CheckRun.CheckSuite.AfterSHA == nil {
		return fmt.Errorf("nil 'check_run.check_suite.after_sha' on check_run event")
	}

	return nil
}

func (ww *WebhookWorker) handlePushEvent(ctx context.Context, deliveryID string, event *github.PushEvent) (*events.APIGatewayV2HTTPResponse, error) {

	if err := validatePushEvent(event); err != nil {
		return &events.APIGatewayV2HTTPResponse{
			StatusCode: http.StatusBadRequest,
			Body:       err.Error(),
		}, nil
	}

	if *event.After == emptyCommit {
		return &events.APIGatewayV2HTTPResponse{
			StatusCode: http.StatusOK,
			Body:       "push event has empty after commit - no event created",
		}, nil
	}

	// Send Message to SNS
	msg := &github_tpb.PushMessage{
		DeliveryId: deliveryID,
		Before:     *event.Before,
		After:      *event.After,
		Ref:        *event.Ref,
		Repo:       *event.Repo.Name,
		Owner:      *event.Repo.Owner.Name,
	}

	return ww.publish(ctx, msg)
}

func (ww *WebhookWorker) publish(ctx context.Context, msg o5msg.Message) (*events.APIGatewayV2HTTPResponse, error) {

	wireMessage, err := o5msg.WrapMessage(msg)
	if err != nil {
		return nil, err
	}

	wireMessage.SourceApp = ww.Source.SourceApp
	wireMessage.SourceEnv = ww.Source.SourceEnv

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

const emptyCommit = "0000000000000000000000000000000000000000"

func validatePushEvent(event *github.PushEvent) error {

	if event.Ref == nil {
		return fmt.Errorf("nil 'ref' on push event")
	}

	if err := validateRepo1(event.Repo); err != nil {
		return err
	}

	if event.After == nil {
		return fmt.Errorf("nil 'after' on push event")
	}

	if event.Before == nil {
		return fmt.Errorf("nil 'before' on push event")
	}
	return nil

}

func validateRepo1(repo *github.PushEventRepository) error {
	if repo == nil {
		return fmt.Errorf("nil 'repo' on check_run event")
	}
	if repo.Owner == nil {
		return fmt.Errorf("nil 'repo.owner' on check_run event")
	}
	if repo.Owner.Name == nil {
		return fmt.Errorf("nil 'repo.owner.name' on check_run event")
	}
	if repo.Name == nil {
		return fmt.Errorf("nil 'repo.name' on check_run event")
	}
	return nil
}

func validateRepo(repo *github.Repository) error {
	if repo == nil {
		return fmt.Errorf("nil 'repo' on check_run event")
	}
	if repo.Owner == nil {
		return fmt.Errorf("nil 'repo.owner' on check_run event")
	}
	if repo.Owner.Login == nil {
		return fmt.Errorf("nil 'repo.owner.login' on check_run event")
	}
	if repo.Name == nil {
		return fmt.Errorf("nil 'repo.name' on check_run event")
	}
	return nil
}
