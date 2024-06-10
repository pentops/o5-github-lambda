package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/pentops/log.go/log"
	"github.com/pentops/o5-github-lambda/internal/github"
	"github.com/pentops/o5-runtime-sidecar/awsmsg"
)

var Version string

func main() {
	ctx := context.Background()
	ctx = log.WithFields(ctx, map[string]interface{}{
		"application": "o5-github-lambda",
		"version":     Version,
	})

	if err := do(ctx); err != nil {
		log.WithError(ctx, err).Error("Failed to serve")
		os.Exit(1)
	}
}

type Secret struct {
	GithubWebhookSecret string `json:"githubWebhookSecret"`
}

func do(ctx context.Context) error {

	sourceConfig := github.SourceConfig{
		SourceApp: os.Getenv("SOURCE_APP"),
		SourceEnv: os.Getenv("SOURCE_ENV"),
	}

	if sourceConfig.SourceApp == "" {
		sourceConfig.SourceApp = "github-webhook"
	}
	if sourceConfig.SourceEnv == "" {
		return fmt.Errorf("SOURCE_ENV is required")
	}

	awsConfig, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}

	// SECRET_ID is the secret created for this webhook in the O5 stack, that it
	// is used for github secrets is a matter for this codebase, not the stack.
	githubSecret := os.Getenv("SECRET_ID")
	if githubSecret == "" {
		return fmt.Errorf("SECRET_ID is required")
	}

	secretsClient := secretsmanager.NewFromConfig(awsConfig)

	secretResp, err := secretsClient.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(githubSecret),
	})
	if err != nil {
		return err
	}

	secretVal := &Secret{}
	if err := json.Unmarshal([]byte(*secretResp.SecretString), secretVal); err != nil {
		return fmt.Errorf("decoding secret: %w", err)
	}

	publishers := []awsmsg.Publisher{}

	targetTopicARN := os.Getenv("TARGET_TOPIC_ARN")
	if targetTopicARN != "" {
		snsClient := sns.NewFromConfig(awsConfig)
		snsPublisher := awsmsg.NewSNSPublisher(snsClient, targetTopicARN)
		publishers = append(publishers, snsPublisher)
	}

	targetEventBusARN := os.Getenv("TARGET_EVENT_BUS_ARN")
	if targetEventBusARN != "" {
		eventBridgeClient := eventbridge.NewFromConfig(awsConfig)
		eventBridgePublisher := awsmsg.NewEventBridgePublisher(eventBridgeClient, targetEventBusARN)
		publishers = append(publishers, eventBridgePublisher)
	}

	webhook, err := github.NewWebhookWorker(secretVal.GithubWebhookSecret, sourceConfig, publishers...)
	if err != nil {
		return err
	}

	lambda.Start(webhook.HandleLambda)
	return nil
}
