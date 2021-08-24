# kindle-converter
Provides a service where you can email files (EPUBs) which are in-compatible with the [
Send to Kindle by E-mail](https://www.amazon.com/gp/sendtokindle/email) service provided by Amazon to a custom email which will convert those files to a compatible format and forward them to your [
Send to Kindle by E-mail](https://www.amazon.com/gp/sendtokindle/email) address

This makes use of 
- AWS Lambda
- AWS SES (Simple Email Service)
- AWS SNS (Simple Notification Service)
- AWS S3
- AWS DynamoDB