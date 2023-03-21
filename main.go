package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

const (
	REGION               = "ap-northeast-2"
	THRESHOLD_PERCENTAGE = 80
	INCREMENT_PERCENTAGE = 30
)

type Response struct {
	StatusCode int    `json:"statusCode"`
	Body       string `json:"body"`
}

func main() {
	lambda.Start(handler)
}

func handler(ctx context.Context) (Response, error) {

	accessKey := os.Getenv("accessKey")
	secretKey := os.Getenv("secretKey")

	var opts []func(options *config.LoadOptions) error
	opts = append(opts, config.WithCredentialsProvider(
		credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")))

	opts = append(opts, config.WithRegion(REGION))

	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		log.Fatalf("unable to load SDK config %s", err)
	}

	ec2Client := ec2.NewFromConfig(cfg)
	ssmClient := ssm.NewFromConfig(cfg)

	volumes, err := getVolumes(ctx, ec2Client)
	if err != nil {
		return Response{}, err
	}

	results := make(chan bool, len(volumes))
	for _, volume := range volumes {
		go processVolume(ctx, volume, results, ec2Client, ssmClient)
	}

	for range volumes {
		<-results
	}

	log.Println("EBS volumes checked and expanded if necessary")
	return Response{
		StatusCode: 200,
		Body:       "EBS volumes checked and expanded if necessary",
	}, nil
}

func processVolume(ctx context.Context, volume types.Volume, results chan<- bool, ec2Client *ec2.Client, ssmClient *ssm.Client) {
	defer func() { results <- true }()

	if len(volume.Attachments) > 0 {
		instanceId := *volume.Attachments[0].InstanceId
		log.Printf("checking instance ID: %v(%v)\n", *volume.Attachments[0].InstanceId, *volume.VolumeId)
		utilization, err := getVolumeUtilizationFromInstance(ctx, ssmClient, instanceId)
		if err != nil {
			log.Printf("error getting volume utilization: %v\n", err)
			return
		}

		if utilization > THRESHOLD_PERCENTAGE {
			handleVolumeExpansion(ctx, volume, ec2Client, ssmClient, instanceId, utilization)
		}
	}
}

func handleVolumeExpansion(ctx context.Context, volume types.Volume, ec2Client *ec2.Client, ssmClient *ssm.Client, instanceId string, utilization int) {
	currentSize, newSIze, err := expandVolume(ctx, ec2Client, ssmClient, &volume, INCREMENT_PERCENTAGE)
	if err != nil {
		log.Printf("Error expanding volume: %v\n", err)
		return
	}

	time.Sleep(time.Second * 5)
	newUtilization, err := getVolumeUtilizationFromInstance(ctx, ssmClient, instanceId)
	if err != nil {
		log.Printf("error getting volume utilization: %v\n", err)
		return
	}

	instanceName, err := getInstanceName(ctx, ec2Client, instanceId)
	if err != nil {
		log.Printf("Error cannot find instance: %v\n", err)
		return
	}

	msg, err := createSlackMsg(instanceName, currentSize, utilization, newSIze, newUtilization, volume)
	if err != nil {
		log.Printf("Error sending slack: %v\n", err)
		return
	}

	slackURL := os.Getenv("slackURL")
	err = send(slackURL, msg)
	if err != nil {
		log.Printf("Error sending slack: %v\n", err)
		return
	}

	log.Printf("expand instance ID: %v(%v)\n", *volume.Attachments[0].InstanceId, *volume.VolumeId)
}

func getVolumes(ctx context.Context, ec2Client *ec2.Client) ([]types.Volume, error) {
	var volumes []types.Volume
	input := &ec2.DescribeVolumesInput{}
	for {
		output, err := ec2Client.DescribeVolumes(ctx, input)
		if err != nil {
			log.Printf("error describing volumes: %v", err)
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

func getVolumeUtilizationFromInstance(ctx context.Context, ssmClient *ssm.Client, instanceId string) (int, error) {
	command := "df --output=pcent / | tail -1 | tr -dc '0-9'"
	input := &ssm.SendCommandInput{
		DocumentName: aws.String("AWS-RunShellScript"),
		InstanceIds:  []string{instanceId},
		Parameters: map[string][]string{
			"commands": {command},
		},
	}

	output, err := ssmClient.SendCommand(ctx, input)
	if err != nil {
		return 0, fmt.Errorf("error sending command to instance: %v", err)
	}

	commandID := output.Command.CommandId
	listResultInput := &ssm.GetCommandInvocationInput{CommandId: commandID, InstanceId: aws.String(instanceId)}

	for {
		res, err := ssmClient.GetCommandInvocation(ctx, listResultInput)
		if err == nil {
			if res.Status == "Success" || res.Status == "Failed" {
				pluginName := "aws:runShellScript"
				outputPlugin := *res.PluginName
				if outputPlugin == pluginName {
					utilizationStr := *res.StandardOutputContent
					utilization, err := strconv.Atoi(utilizationStr)
					if err != nil {
						return 0, fmt.Errorf("error parsing utilization percentage: %v", err)
					}
					return utilization, nil
				}
			}
		}
	}
}

func expandVolume(ctx context.Context, ec2Client *ec2.Client, ssmClient *ssm.Client, volume *types.Volume, incrementPercentage int) (int32, int64, error) {
	currentSize := *volume.Size
	newSize := int64(float64(currentSize) * (1 + float64(incrementPercentage)/100))

	modifyVolumeInput := &ec2.ModifyVolumeInput{
		VolumeId: volume.VolumeId,
		Size:     aws.Int32(int32(newSize)),
	}

	_, err := ec2Client.ModifyVolume(ctx, modifyVolumeInput)
	if err != nil {
		return currentSize, newSize, fmt.Errorf("error modifying volume size: %v", err)
	}

	err = waitUntilVolumeAvailable(ctx, ec2Client, *volume.VolumeId)
	if err != nil {
		return currentSize, newSize, fmt.Errorf("error waiting for volume to be available: %v", err)
	}

	instanceId := *volume.Attachments[0].InstanceId
	device := *volume.Attachments[0].Device

	checkFileSystemCommand := fmt.Sprintf("sudo lsblk -f %s1 -o FSTYPE | tail -n 1", device)
	resizeExtFileSystemCommand := fmt.Sprintf("sudo growpart %s 1 && sudo resize2fs %s1", device, device)
	resizeXfsFileSystemCommand := fmt.Sprintf("sudo growpart %s 1 && sudo xfs_growfs %s1", device, device)

	ssmInput := &ssm.SendCommandInput{
		DocumentName: aws.String("AWS-RunShellScript"),
		InstanceIds:  []string{instanceId},
		Parameters: map[string][]string{
			"commands": {
				fmt.Sprintf("fileSystem=$( %s )", checkFileSystemCommand),
				fmt.Sprintf("if [ \"$fileSystem\" = \"xfs\" ]; then %s; else %s; fi", resizeXfsFileSystemCommand, resizeExtFileSystemCommand),
			},
		},
	}

	_, err = ssmClient.SendCommand(ctx, ssmInput)
	if err != nil {
		return currentSize, newSize, fmt.Errorf("error sending resize file system command to instance: %v", err)
	}

	return currentSize, newSize, nil
}

func waitUntilVolumeAvailable(ctx context.Context, ec2Client *ec2.Client, volumeID string) error {
	describeVolumesModificationsInput := &ec2.DescribeVolumesModificationsInput{
		VolumeIds: []string{volumeID},
	}

	for {
		fmt.Println("waiting")
		output, err := ec2Client.DescribeVolumesModifications(ctx, describeVolumesModificationsInput)
		if err != nil {
			return fmt.Errorf("error describing volume: %v", err)
		}

		if len(output.VolumesModifications) > 0 {
			fmt.Println(output.VolumesModifications[0].ModificationState)
			if output.VolumesModifications[0].ModificationState == types.VolumeModificationStateOptimizing {
				fmt.Println(output.VolumesModifications[0].ModificationState)
				break
			}
		}

		time.Sleep(5 * time.Second)
	}

	return nil
}

func getInstanceName(ctx context.Context, ec2Client *ec2.Client, instanceId string) (string, error) {
	input := ec2.DescribeInstancesInput{InstanceIds: []string{instanceId}}
	result, err := ec2Client.DescribeInstances(ctx, &input)
	if err != nil {
		return "", fmt.Errorf("error cannot find instance: %v", err)
	}

	for _, reservation := range result.Reservations {
		for _, instance := range reservation.Instances {
			for _, tag := range instance.Tags {
				if *tag.Key == "Name" {
					instanceName := *tag.Value
					return instanceName, nil
				}
			}
		}
	}

	return "", fmt.Errorf("error cannot find instance")
}

type AttachmentField struct {
	Title string `json:"title"`
	Value string `json:"value"`
	Short bool   `json:"short"`
}

type AttachmentAction struct {
	Type string `json:"type"`
	Text string `json:"text"`
	Url  string `json:"url"`
}

type Attachment struct {
	Fallback *string `json:"fallback,omitempty"`
	Color    *string `json:"color,omitempty"`

	CallbackID *string `json:"callback_id,omitempty"`

	AuthorName *string `json:"author_name,omitempty"`
	AuthorLink *string `json:"author_link,omitempty"`
	AuthorIcon *string `json:"author_icon,omitempty"`

	PreText   *string `json:"pretext,omitempty"`
	Title     *string `json:"title,omitempty"`
	TitleLink *string `json:"title_link,omitempty"`
	Text      *string `json:"text,omitempty"`

	ImageURL   *string             `json:"image_url,omitempty"`
	Fields     []*AttachmentField  `json:"fields,omitempty"`
	TS         *int64              `json:"ts,omitempty"`
	MarkdownIn *[]string           `json:"mrkdwn_in,omitempty"`
	Actions    []*AttachmentAction `json:"actions,omitempty"`

	Footer     *string `json:"footer,omitempty"`
	FooterIcon *string `json:"footer_icon,omitempty"`
}

type Payload struct {
	Username        string       `json:"username,omitempty"`
	IconEmoji       string       `json:"icon_emoji,omitempty"`
	IconURL         string       `json:"icon_url,omitempty"`
	Channel         string       `json:"channel,omitempty"`
	ThreadTimestamp string       `json:"thread_ts,omitempty"`
	Text            string       `json:"text,omitempty"`
	Attachments     []Attachment `json:"attachments,omitempty"`
	Parse           string       `json:"parse,omitempty"`
}

func (a *Attachment) AddField(field AttachmentField) *Attachment {
	a.Fields = append(a.Fields, &field)
	return a
}

func (a *Attachment) AddColor(color string) *Attachment {
	a.Color = &color
	return a
}

func (a *Attachment) AddAction(action AttachmentAction) *Attachment {
	a.Actions = append(a.Actions, &action)
	return a
}

func getKstTime() (string, error) {
	loc, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		return "", err
	}

	kst := time.Now().In(loc)
	formattedTime := kst.Format("2006-01-02 15:04")
	return formattedTime, nil
}

func createSlackMsg(instanceName string, currentSize int32, utilization int, newSize int64, newUtilization int, volume types.Volume) (Payload, error) {
	time, err := getKstTime()
	if err != nil {
		return Payload{}, err
	}

	attachment := Attachment{}
	attachment.AddColor("good")
	attachment.AddField(AttachmentField{Title: "시간", Value: time, Short: true})
	attachment.AddField(AttachmentField{Title: "변경 사항", Value: "EBS 볼륨 용량 확장", Short: true})
	attachment.AddField(AttachmentField{Title: "EBS ID", Value: *volume.VolumeId, Short: true})
	attachment.AddField(AttachmentField{Title: "인스턴스 ID", Value: *volume.Attachments[0].InstanceId, Short: true})
	attachment.AddField(AttachmentField{Title: "이전 용량", Value: fmt.Sprintf("%dG(%d 퍼센트 사용)", currentSize, utilization), Short: true})
	attachment.AddField(AttachmentField{Title: "변경 용량", Value: fmt.Sprintf("%dG(%d 퍼센트 사용)", newSize, newUtilization), Short: true})

	return Payload{
		Text:        fmt.Sprintf("%s: 용량 증설", fmt.Sprintf("%s", instanceName)),
		Attachments: []Attachment{attachment},
	}, nil
}

func send(slackURL string, payload Payload) error {
	payloadJson, _ := json.Marshal(payload)
	resp, err := http.Post(slackURL, "application/json", bytes.NewBuffer(payloadJson))
	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("send Fail Slack Alert :%v", resp.StatusCode)
	}

	return nil
}
