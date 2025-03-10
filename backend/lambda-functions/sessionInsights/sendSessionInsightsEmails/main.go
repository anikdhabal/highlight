package main

import (
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/highlight-run/highlight/backend/lambda-functions/sessionInsights/handlers"
	"github.com/highlight/highlight/sdk/highlight-go"
	hlog "github.com/highlight/highlight/sdk/highlight-go/log"
	"os"
)

var h handlers.Handlers

func init() {
	h = handlers.NewHandlers()
}

func main() {
	highlight.SetProjectID("1jdkoe52")
	highlight.Start(
		highlight.WithServiceName("lambda-functions--sendSessionInsightsEmails"),
		highlight.WithServiceVersion(os.Getenv("REACT_APP_COMMIT_SHA")),
	)
	hlog.Init()
	lambda.StartWithOptions(
		h.SendSessionInsightsEmails,
		lambda.WithEnableSIGTERM(highlight.Stop),
	)
}
