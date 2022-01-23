package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/DusanKasan/parsemail"
	"github.com/aws/aws-lambda-go/events"
	runtime "github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbattribute"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/aws/aws-sdk-go/service/ses"
	"gopkg.in/gomail.v2"
)

const (
	Sender            = "conversion@noreply.kindle.vangel.io"
	BaseReceiveDomain = "@kindle.vangel.io"
	TableName         = "KindleConverter"
	Folder            = "/tmp"
	// Base64 adds a large amount of overhead up to 36%. Adding 4% extra to account for other data
	MaxEmailSize = int64(10485760 * .6)
)

var (
	s3Client       *s3.S3
	dynamodbClient *dynamodb.DynamoDB
	sesClient      *ses.SES
)

type RecipientResult struct {
	RecipientEmail    string
	ApprovedEmails    []string
	DestinationEmails []string
}

type FileToSend struct {
	LocalFilename   string
	DisplayFilename string
	FileSize        int64
}

func (a BySize) Len() int           { return len(a) }
func (a BySize) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a BySize) Less(i, j int) bool { return a[i].FileSize < a[j].FileSize }

type BySize []FileToSend

// Extracted from https://www.amazon.com/gp/sendtokindle/email
func getAlreadySupportedFileTypes() []string {
	return []string{".doc", ".docx", ".rtf", ".htm", ".html", ".txt", ".mobi", ".azw", ".azw3", ".azw4", ".pdf", ".jpg", ".jpeg", ".gif", ".bmp", ".png"}
}

// Extracted from https://github.com/kovidgoyal/calibre/blob/5993017df9b7bd9286483b063ed4a035c94b6a82/manual/faq.rst
func getSupportedConversionFileTypes() []string {
	return []string{".azw", ".azw3", ".azw4", ".cbz", ".cbr", ".cb7", ".cbc", ".chm", ".djvu", ".docx", ".epub", ".fb2", ".fbz", ".html", ".htmlz", ".lit", ".lrf", ".mobi", ".odt", ".pdf", ".prc", ".pdb", ".pml", ".rb", ".rtf", ".snb", ".tcr", ".txt", ".txtz"}
}

// This function deletes the stored email then passes the error through
func handleError(bucketName, objectKey string, err error) error {
	// Delete email stored in s3 bucket
	s3Client.DeleteObject(&s3.DeleteObjectInput{Bucket: aws.String(bucketName), Key: aws.String(objectKey)})
	return err
}

func emailOptimizer(files []FileToSend) [10][]FileToSend {
	sort.Sort(sort.Reverse(BySize(files)))
	optimizedEmails := [10][]FileToSend{}
	for _, file := range files {
		if file.FileSize > MaxEmailSize {
			continue
		}
		for emailIndex := range optimizedEmails {
			currentEmailSize := emailSize(optimizedEmails[emailIndex])
			if currentEmailSize+file.FileSize < MaxEmailSize {
				optimizedEmails[emailIndex] = append(optimizedEmails[emailIndex], file)
				break
			}
		}
	}
	return optimizedEmails
}

func emailSize(files []FileToSend) int64 {
	emailSize := int64(0)
	for _, file := range files {
		emailSize += file.FileSize
	}
	return emailSize
}

func handleRequest(ctx context.Context, event events.SNSEvent) error {
	// Iterate over all records in event
	for _, record := range event.Records {
		// Unmarshal message into map[string]interface{}
		var message map[string]interface{}
		json.Unmarshal([]byte(record.SNS.Message), &message)
		// Extract required information from message
		objectKey := message["receipt"].(map[string]interface{})["action"].(map[string]interface{})["objectKey"].(string)
		bucketName := message["receipt"].(map[string]interface{})["action"].(map[string]interface{})["bucketName"].(string)
		destinations := message["mail"].(map[string]interface{})["destination"].([]interface{})
		source := message["mail"].(map[string]interface{})["source"].(string)

		// If email was sent to multiple people extract the email we recieved on
		intendedDestination := ""
		for _, destination := range destinations {
			if strings.HasSuffix(destination.(string), BaseReceiveDomain) {
				intendedDestination = destination.(string)
			}
		}
		// Attempt to get configuration related to email the documents were sent to
		recipientResult, err := dynamodbClient.GetItem(&dynamodb.GetItemInput{
			TableName: aws.String(TableName),
			Key: map[string]*dynamodb.AttributeValue{
				"RecipientEmail": {
					S: aws.String(intendedDestination),
				},
			},
		})
		// If no configuration is found
		if recipientResult.Item == nil {
			return handleError(objectKey, bucketName, err)
		}

		item := RecipientResult{}
		// Unmarshal config into a RecipientResult struct
		err = dynamodbattribute.UnmarshalMap(recipientResult.Item, &item)
		if err != nil {
			return handleError(objectKey, bucketName, errors.New(fmt.Sprintf("Failed to unmarshal Record, %v", err)))
		}
		// If email recieved from is not on the ApprovedEmails list reject the conversion attempt
		if !contains(item.ApprovedEmails, source) {
			return handleError(objectKey, bucketName, errors.New("unapproved email"))
		}

		// Download email from S3 and store in aws.WriteAtBuffer
		emailBuffer := &aws.WriteAtBuffer{}
		downloader := s3manager.NewDownloader(session.New())
		_, err = downloader.Download(emailBuffer, &s3.GetObjectInput{
			Bucket: aws.String(bucketName),
			Key:    aws.String(objectKey),
		})
		if err != nil {
			return handleError(objectKey, bucketName, err)
		}
		// Parse email
		email, err := parsemail.Parse(bytes.NewReader(emailBuffer.Bytes()))
		if err != nil {
			return handleError(objectKey, bucketName, err)
		}
		// List of files that will be sent to destination email
		filesToSend := []FileToSend{}
		for i, a := range email.Attachments {
			// Extract attachment and save to disk
			fileLocation := Folder + "/" + "book" + strconv.Itoa(i) + filepath.Ext(a.Filename)
			f, err := os.Create(fileLocation)
			if err != nil {
				return handleError(objectKey, bucketName, err)
			}
			defer f.Close()
			fileSize, err := io.Copy(f, a.Data)
			if err != nil {
				return handleError(objectKey, bucketName, err)
			}
			f.Close()
			// If file is already supported by the Amazon Kindle email service skip conversion
			if contains(getAlreadySupportedFileTypes(), strings.ToLower(filepath.Ext(a.Filename))) {
				filesToSend = append(filesToSend, FileToSend{LocalFilename: fileLocation, DisplayFilename: a.Filename, FileSize: fileSize})
				continue
			}
			// If file isn't supported by the Amazon Kindle email service and we can't convert skip the file
			if !contains(getSupportedConversionFileTypes(), strings.ToLower(filepath.Ext(a.Filename))) {
				continue
			}
			// Convert all files sent to us to mobi
			convertedFileLocation := Folder + "/" + "book" + strconv.Itoa(i) + ".mobi"

			_, err = exec.Command("ebook-convert", fileLocation, convertedFileLocation).CombinedOutput()
			if err != nil {
				return handleError(objectKey, bucketName, err)
			}

			// Stat converted file
			fi, err := os.Stat(convertedFileLocation)
			if err != nil {
				return handleError(objectKey, bucketName, err)
			}
			// Get size of file
			fileSizeConverted := fi.Size()

			filesToSend = append(filesToSend, FileToSend{LocalFilename: convertedFileLocation, DisplayFilename: a.Filename[0:len(a.Filename)-len(filepath.Ext(a.Filename))] + ".mobi", FileSize: fileSizeConverted})

		}
		optimizedEmails := emailOptimizer(filesToSend)
		for destinationEmailIndex := range item.DestinationEmails {
			for emailIndex := range optimizedEmails {
				if !(len(optimizedEmails[emailIndex]) > 0) {
					break
				}
				// Create new raw email
				msg := gomail.NewMessage()
				msg.SetHeader("From", Sender)
				msg.SetHeader("To", item.DestinationEmails[destinationEmailIndex])
				msg.SetHeader("Subject", "")
				msg.SetBody("text/html", "")
				for _, fileToSend := range optimizedEmails[emailIndex] {
					// File is attached using local filename but renamed to display filename
					msg.Attach(fileToSend.LocalFilename, gomail.Rename(fileToSend.DisplayFilename))
				}

				var emailRaw bytes.Buffer
				msg.WriteTo(&emailRaw)
				// Prepare raw email
				input := &ses.SendRawEmailInput{
					Destinations: []*string{aws.String(item.DestinationEmails[destinationEmailIndex])},
					Source:       aws.String(Sender),
					RawMessage: &ses.RawMessage{
						Data: emailRaw.Bytes(),
					},
				}
				// Send email
				_, err = sesClient.SendRawEmail(input)
				if err != nil {
					return handleError(objectKey, bucketName, err)
				}
			}
		}

		// Delete email stored in s3 bucket
		s3Client.DeleteObject(&s3.DeleteObjectInput{Bucket: aws.String(bucketName), Key: aws.String(objectKey)})

	}
	return nil
}

func main() {
	// Initalize session and clients
	sess, err := session.NewSession()
	if err != nil {
		panic(err)
	}
	s3Client = s3.New(sess)
	dynamodbClient = dynamodb.New(sess)
	sesClient = ses.New(sess)
	// Pass request to handler
	runtime.Start(handleRequest)
}

func contains(s []string, str string) bool {
	for _, v := range s {
		if v == str {
			return true
		}
	}
	return false
}
