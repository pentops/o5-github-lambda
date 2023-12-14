package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sns/types"
	"github.com/pentops/o5-github-lambda/github"
	"google.golang.org/protobuf/encoding/protojson"
	"gopkg.daemonl.com/envconf"
	"gopkg.daemonl.com/log"
)

type envConfig struct {
	SecretId       string `env:"SECRET_ID"`
	TargetTopicARN string `env:"TARGET_TOPIC_ARN"`
}

var Version string

func main() {
	ctx := context.Background()
	ctx = log.WithFields(ctx, map[string]interface{}{
		"application": "o5-github-lambda",
		"version":     Version,
	})

	config := envConfig{}
	if err := envconf.Parse(&config); err != nil {
		log.WithError(ctx, err).Error("Failed to load config")
		os.Exit(1)
	}

	if err := do(ctx, config); err != nil {
		log.WithError(ctx, err).Error("Failed to serve")
		os.Exit(1)
	}
}

type Secret struct {
	GithubWebhookSecret string `json:"githubWebhookSecret"`
}

func do(ctx context.Context, cfg envConfig) error {

	awsConfig, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}

	snsClient := sns.NewFromConfig(awsConfig)
	secretsClient := secretsmanager.NewFromConfig(awsConfig)

	secretResp, err := secretsClient.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(cfg.SecretId),
	})
	if err != nil {
		return err
	}

	secretVal := &Secret{}
	if err := json.Unmarshal([]byte(*secretResp.SecretString), secretVal); err != nil {
		return fmt.Errorf("decoding secret: %w", err)
	}

	snsPublisher := NewSNSPublisher(snsClient, cfg.TargetTopicARN)

	webhook, err := github.NewWebhookWorker(secretVal.GithubWebhookSecret, snsPublisher)
	if err != nil {
		return err
	}

	lambda.Start(webhook.HandleLambda)
	return nil
}

type SNSAPI interface {
	Publish(ctx context.Context, params *sns.PublishInput, optFns ...func(*sns.Options)) (*sns.PublishOutput, error)
}

type SNSPublisher struct {
	client   SNSAPI
	topicARN string
}

func NewSNSPublisher(client SNSAPI, topicARN string) *SNSPublisher {
	return &SNSPublisher{
		client:   client,
		topicARN: topicARN,
	}
}

func (p *SNSPublisher) Publish(ctx context.Context, message github.Message) error {
	asBytes, err := protojson.Marshal(message)
	if err != nil {
		return err
	}

	attributes := map[string]types.MessageAttributeValue{}

	attributes["Content-Type"] = types.MessageAttributeValue{
		DataType:    aws.String("String"),
		StringValue: aws.String("application/json"),
	}

	for k, v := range message.MessagingHeaders() {
		attributes[k] = types.MessageAttributeValue{
			DataType:    aws.String("String"),
			StringValue: aws.String(v),
		}
	}

	input := &sns.PublishInput{
		Message:           aws.String(string(asBytes)), // Standard plain text utf-8
		TopicArn:          &p.topicARN,
		MessageAttributes: attributes,
		Subject:           aws.String(message.MessagingTopic()),
	}
	_, err = p.client.Publish(ctx, input)
	return err
}
