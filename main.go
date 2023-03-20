package main

import (
	"context"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"log"
	"os"
	"strconv"
)

const (
	REGION               = "ap-northeast-2"
	THRESHOLD_PERCENTAGE = 80
	INCREMENT_PERCENTAGE = 20
)

type Response struct {
	StatusCode int    `json:"statusCode"`
	Body       string `json:"body"`
}

func main() {
	lambda.Start(handler)
}

func handler(ctx context.Context) (Response, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(REGION))
	if err != nil {
		log.Fatalf("unable to load SDK config %s", err)
	}

	ec2Client := ec2.NewFromConfig(cfg)
	ssmClient := ssm.NewFromConfig(cfg)

	volumes, err := getVolumes(ctx, ec2Client)
	if err != nil {
		return Response{}, err
	}

	for _, volume := range volumes {
		volume.
			utilization, err :=
	}
}

func getVolumes(ctx context.Context, ec2Client *ec2.Client) ([]types.Volume, error) {
	var volumes []types.Volume
	input := &ec2.DescribeVolumesInput{}
	for {
		output, err := ec2Client.DescribeVolumes(ctx, input)
		if err != nil {
			log.Printf("Error describing volumes: %v", err)
			return nil, err
		}

		volumes = append(volumes, output.Volumes...)

		if output.NextToken == nil {
			break
		}

		input.NextToken = output.NextToken
	}

	return volumes, nil
}

func GetVolumeUtilizationFromInstance(ctx context.Context, ssmClient *ssm.Client, instanceId string) (float64, error) {
	command := "df --output=pcent / | tail -1 | tr -dc '0-9'"
	intput := &ssm.SendCommandInput{
		DocumentName: aws.String("AWS-RunShellScript"),
		InstanceIds:  []string{instanceId},
		Parameters: map[string][]string{
			"command": {command},
		},
	}


}
