package graph

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	hubspotApi "github.com/highlight-run/highlight/backend/hubspot"

	github2 "github.com/google/go-github/v50/github"
	parse "github.com/highlight-run/highlight/backend/event-parse"
	"github.com/highlight-run/highlight/backend/integrations/github"
	"github.com/sashabaranov/go-openai"

	"gorm.io/gorm/clause"

	"github.com/go-chi/chi"
	"github.com/leonelquinteros/hubspot"
	"github.com/samber/lo"

	"github.com/highlight-run/go-resthooks"
	"github.com/highlight-run/highlight/backend/alerts/integrations/discord"
	"github.com/highlight-run/highlight/backend/clickhouse"
	"github.com/highlight-run/highlight/backend/clickup"
	"github.com/highlight-run/highlight/backend/front"
	"github.com/highlight-run/highlight/backend/integrations"
	"github.com/highlight-run/highlight/backend/integrations/height"
	kafka_queue "github.com/highlight-run/highlight/backend/kafka-queue"
	"github.com/highlight-run/highlight/backend/lambda"
	"github.com/highlight-run/highlight/backend/oauth"
	"github.com/highlight-run/highlight/backend/redis"
	"github.com/highlight-run/highlight/backend/stepfunctions"
	"github.com/highlight-run/highlight/backend/store"
	"github.com/highlight-run/highlight/backend/vercel"

	"github.com/pkg/errors"

	"github.com/clearbit/clearbit-go/clearbit"
	"github.com/golang-jwt/jwt/v4"
	"github.com/openlyinc/pointy"
	e "github.com/pkg/errors"
	"github.com/sendgrid/sendgrid-go"
	"github.com/sendgrid/sendgrid-go/helpers/mail"
	log "github.com/sirupsen/logrus"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/stripe/stripe-go/v72"
	"github.com/stripe/stripe-go/v72/client"
	"github.com/stripe/stripe-go/v72/webhook"
	"gorm.io/gorm"

	"github.com/highlight-run/workerpool"

	Email "github.com/highlight-run/highlight/backend/email"
	"github.com/highlight-run/highlight/backend/embeddings"
	"github.com/highlight-run/highlight/backend/model"
	"github.com/highlight-run/highlight/backend/pricing"
	modelInputs "github.com/highlight-run/highlight/backend/private-graph/graph/model"
	"github.com/highlight-run/highlight/backend/storage"
	"github.com/highlight-run/highlight/backend/timeseries"
	"github.com/highlight-run/highlight/backend/util"
	"github.com/highlight/highlight/sdk/highlight-go"
)

// This file will not be regenerated automatically.
//
// It serves as dependency injection for your app, add any dependencies you require here.

const ErrorGroupLookbackDays = 7
const SessionActiveMetricName = "sessionActiveLength"
const SessionProcessedMetricName = "sessionProcessed"

var AuthenticationError = errors.New("401 - AuthenticationError")
var AuthorizationError = errors.New("403 - AuthorizationError")

var (
	WhitelistedUID  = os.Getenv("WHITELISTED_FIREBASE_ACCOUNT")
	JwtAccessSecret = os.Getenv("JWT_ACCESS_SECRET")
	FrontendURI     = os.Getenv("FRONTEND_URI")
)

var BytesConversion = map[string]int64{
	"b":  1,
	"kb": 1024,
	"mb": 1024 * 1024,
	"gb": 1024 * 1024 * 1024,
	"tb": 1024 * 1024 * 1024 * 1024,
	"pb": 1024 * 1024 * 1024 * 1024 * 1024,
}

type PromoCode struct {
	TrialDays  int
	ValidUntil time.Time
}

var PromoCodes = map[string]PromoCode{
	"WEBDEVSIMPLIFIED": {
		TrialDays:  60,
		ValidUntil: time.Date(2023, time.May, 15, 0, 0, 0, 0, time.UTC),
	},
	"CATCHMYERRORS": {
		TrialDays:  7,
		ValidUntil: time.Date(2023, time.January, 17, 0, 0, 0, 0, time.UTC),
	},
	"SIMPLIFIEDHIGHLIGHT": {
		TrialDays:  60,
		ValidUntil: time.Date(2023, time.August, 7, 0, 0, 0, 0, time.UTC),
	},
	"USEGOLANG": {
		TrialDays:  14,
		ValidUntil: time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC),
	},
}

func isAuthError(err error) bool {
	return e.Is(err, AuthenticationError) || e.Is(err, AuthorizationError)
}

type Resolver struct {
	DB                     *gorm.DB
	TDB                    timeseries.DB
	MailClient             *sendgrid.Client
	StripeClient           *client.API
	StorageClient          storage.Client
	LambdaClient           *lambda.Client
	ClearbitClient         *clearbit.Client
	PrivateWorkerPool      *workerpool.WorkerPool
	SubscriptionWorkerPool *workerpool.WorkerPool
	RH                     *resthooks.Resthook
	HubspotApi             hubspotApi.Api
	Redis                  *redis.Client
	StepFunctions          *stepfunctions.Client
	OAuthServer            *oauth.Server
	IntegrationsClient     *integrations.Client
	ClickhouseClient       *clickhouse.Client
	Store                  *store.Store
	DataSyncQueue          kafka_queue.MessageQueue
	TracesQueue            kafka_queue.MessageQueue
	EmbeddingsClient       embeddings.Client
}

func (r *mutationResolver) Transaction(body func(txnR *mutationResolver) error) error {
	return r.DB.Transaction(func(tx *gorm.DB) error {
		transactionR := *r
		embedded := *transactionR.Resolver
		transactionR.Resolver = &embedded
		transactionR.Resolver.DB = tx

		return body(&transactionR)
	})
}

func (r *Resolver) createAdmin(ctx context.Context) (*model.Admin, error) {
	adminUID := pointy.String(fmt.Sprintf("%v", ctx.Value(model.ContextKeys.UID)))
	firebaseSpan, _ := util.StartSpanFromContext(ctx, "resolver.createAdmin", util.ResourceName("db.createAdminFromFirebase"),
		util.Tag("admin_uid", adminUID))
	firebaseUser, err := AuthClient.GetUser(context.Background(), *adminUID)
	if err != nil {
		spanError := e.Wrap(err, "error retrieving user from firebase api")
		firebaseSpan.Finish(spanError)
		return nil, spanError
	}
	firebaseSpan.Finish()

	adminSpan, _ := util.StartSpanFromContext(ctx, "resolver.createAdmin", util.ResourceName("db.admin"))
	admin := &model.Admin{
		UID:                   adminUID,
		Name:                  &firebaseUser.DisplayName,
		Email:                 &firebaseUser.Email,
		PhotoURL:              &firebaseUser.PhotoURL,
		EmailVerified:         &firebaseUser.EmailVerified,
		Phone:                 &firebaseUser.PhoneNumber,
		AboutYouDetailsFilled: &model.F,
	}
	tx := r.DB.Where(&model.Admin{UID: admin.UID}).
		Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "uid"}}, DoNothing: true}).
		Create(&admin).
		Attrs(&admin)
	if tx.Error != nil {
		spanError := e.Wrap(tx.Error, "error retrieving user from db")
		adminSpan.Finish(spanError)
		return nil, spanError
	}
	adminSpan.Finish()

	return admin, nil
}

func (r *Resolver) getCurrentAdmin(ctx context.Context) (*model.Admin, error) {
	admin, err := r.Query().Admin(ctx)
	if err != nil {
		return nil, AuthenticationError
	}

	return admin, nil
}

func (r *Resolver) getCustomVerifiedAdminEmailDomain(admin *model.Admin) (string, error) {
	domain, err := r.getVerifiedAdminEmailDomain(admin)
	if err != nil {
		return "", e.Wrap(err, "error getting verified admin email domain")
	}

	// this is just the top 10 email domains as of June 6, 2016, and protonmail.com
	if map[string]bool{"gmail.com": true, "yahoo.com": true, "hotmail.com": true, "aol.com": true, "hotmail.co.uk": true, "protonmail.com": true, "hotmail.fr": true, "msn.com": true, "yahoo.fr": true, "wanadoo.fr": true, "orange.fr": true}[strings.ToLower(domain)] {
		return "", nil
	}

	return domain, nil
}

func (r *Resolver) getVerifiedAdminEmailDomain(admin *model.Admin) (string, error) {
	if admin.EmailVerified == nil || !*admin.EmailVerified {
		return "", e.New("admin email is not verified")
	}
	if admin.Email == nil {
		return "", e.New("admin email is nil")
	}
	components := strings.Split(*admin.Email, "@")
	if len(components) < 2 {
		return "", e.New("invalid admin email")
	}
	domain := components[1]
	return domain, nil
}

func (r *Resolver) getTaggedAdmins(taggedAdmins []*modelInputs.SanitizedAdminInput, isGuestCreatingSession bool) (admins []model.Admin) {
	if !isGuestCreatingSession {
		for _, a := range taggedAdmins {
			admins = append(admins,
				model.Admin{
					Model: model.Model{ID: a.ID},
				},
			)
		}
	}
	return
}

func (r *Resolver) formatSanitizedAuthor(admin *model.Admin) *modelInputs.SanitizedAdmin {
	name := ""
	email := ""
	photo_url := ""

	if admin.Name != nil {
		name = *admin.Name
	}
	if admin.Email != nil {
		email = *admin.Email
	}
	if admin.PhotoURL != nil {
		photo_url = *admin.PhotoURL
	}

	sanitizedAdmin := &modelInputs.SanitizedAdmin{
		ID:       admin.ID,
		Name:     &name,
		Email:    email,
		PhotoURL: &photo_url,
	}

	return sanitizedAdmin
}

func (r *Resolver) isWhitelistedAccount(ctx context.Context) bool {
	uid := fmt.Sprintf("%v", ctx.Value(model.ContextKeys.UID))
	email := fmt.Sprintf("%v", ctx.Value(model.ContextKeys.Email))
	// Allow access to engineering@highlight.run or any verified @highlight.run / @runhighlight.com email.
	_, isAdmin := lo.Find(HighlightAdminEmailDomains, func(domain string) bool { return strings.Contains(email, domain) })
	isDockerDefaultAccount := util.IsInDocker() && email == "demo@example.com"
	return isAdmin || uid == WhitelistedUID || isDockerDefaultAccount
}

func (r *Resolver) isDemoProject(ctx context.Context, project_id int) bool {
	return project_id == r.demoProjectID(ctx)
}

func (r *Resolver) isDemoWorkspace(workspace_id int) bool {
	return workspace_id == 0
}

func (r *Resolver) demoProjectID(ctx context.Context) int {
	demoProjectString := os.Getenv("DEMO_PROJECT_ID")

	// Demo project is disabled if the env var is not set.
	if demoProjectString == "" {
		return 0
	}

	if demoProjectID, err := strconv.Atoi(demoProjectString); err != nil {
		log.WithContext(ctx).Error(err, "error converting DemoProjectID to int")
		return 0
	} else {
		return demoProjectID
	}
}

// These are authentication methods used to make sure that data is secured.
// This'll probably get expensive at some point; they can probably be cached.

// isAdminInProjectOrDemoProject should be used for actions that you want admins in all projects
// and laymen in the demo project to have access to.
func (r *Resolver) isAdminInProjectOrDemoProject(ctx context.Context, project_id int) (*model.Project, error) {
	authSpan, _ := util.StartSpanFromContext(ctx, "resolver.internal.auth", util.ResourceName("isAdminInProjectOrDemoProject"))
	defer authSpan.Finish()
	start := time.Now()
	defer func() {
		highlight.RecordMetric(
			ctx, "resolver.internal.auth.isAdminInProjectOrDemoProject", time.Since(start).Seconds(),
		)
	}()
	var project *model.Project
	var err error
	if r.isDemoProject(ctx, project_id) {
		if err = r.DB.Model(&model.Project{}).Where("id = ?", r.demoProjectID(ctx)).Take(&project).Error; err != nil {
			return nil, e.Wrap(err, "error querying demo project")
		}
	} else {
		project, err = r.isAdminInProject(ctx, project_id)
		if err != nil {
			return nil, err
		}
	}
	return project, nil
}

func (r *Resolver) isAdminInWorkspaceOrDemoWorkspace(ctx context.Context, workspace_id int) (*model.Workspace, error) {
	authSpan, _ := util.StartSpanFromContext(ctx, "resolver.internal.auth", util.ResourceName("isAdminInWorkspaceOrDemoWorkspace"))
	defer authSpan.Finish()
	start := time.Now()
	defer func() {
		highlight.RecordMetric(
			ctx, "resolver.internal.auth.isAdminInWorkspaceOrDemoWorkspace", time.Since(start).Seconds(),
		)
	}()
	var workspace *model.Workspace
	var err error
	if r.isDemoWorkspace(workspace_id) {
		if err = r.DB.Model(&model.Workspace{}).Where("id = ?", 0).Take(&workspace).Error; err != nil {
			return nil, e.Wrap(err, "error querying demo workspace")
		}
	} else {
		workspace, err = r.isAdminInWorkspace(ctx, workspace_id)
		if err != nil {
			return nil, err
		}
	}
	return workspace, nil
}

func (r *Resolver) GetWorkspace(workspaceID int) (*model.Workspace, error) {
	var workspace model.Workspace
	if err := r.DB.Where(&model.Workspace{Model: model.Model{ID: workspaceID}}).Take(&workspace).Error; err != nil {
		return nil, e.Wrap(err, "error querying workspace")
	}
	return &workspace, nil
}

func (r *Resolver) GetAdminRole(ctx context.Context, adminID int, workspaceID int) (string, error) {
	var workspaceAdmin model.WorkspaceAdmin
	if err := r.DB.Where(&model.WorkspaceAdmin{AdminID: adminID, WorkspaceID: workspaceID}).Take(&workspaceAdmin).Error; err != nil {
		return "", e.Wrap(err, "error querying workspace_admin")
	}
	if workspaceAdmin.Role == nil || *workspaceAdmin.Role == "" {
		log.WithContext(ctx).Errorf("workspace_admin admin_id:%d,workspace_id:%d has invalid role", adminID, workspaceID)
		return "", e.New("workspace_admin has invalid role")

	}
	return *workspaceAdmin.Role, nil
}

func (r *Resolver) addAdminMembership(ctx context.Context, workspaceId int, inviteID string) (*int, error) {
	workspace := &model.Workspace{}
	if err := r.DB.Model(workspace).Where("id = ?", workspaceId).Take(workspace).Error; err != nil {
		return nil, e.Wrap(err, "500: error querying workspace")
	}
	admin, err := r.getCurrentAdmin(ctx)
	if err != nil {
		return nil, err
	}

	inviteLink := &model.WorkspaceInviteLink{}
	if err := r.DB.Where(&model.WorkspaceInviteLink{WorkspaceID: &workspaceId, Secret: &inviteID}).Take(&inviteLink).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, e.New("404: Invite not found")
		}
		return nil, e.Wrap(err, "500: error querying for invite Link")
	}

	// Non-admin specific invites don't have a specific invitee. Only block if the invite is for a specific admin and the emails don't match.
	if inviteLink.InviteeEmail != nil {
		// check case-insensitively because email addresses are case-insensitive.
		if !strings.EqualFold(*inviteLink.InviteeEmail, *admin.Email) {
			return nil, AuthorizationError
		}
	}

	if r.IsInviteLinkExpired(inviteLink) {
		if err := r.DB.Delete(inviteLink).Error; err != nil {
			return nil, e.Wrap(err, "500: error while trying to delete expired invite link")
		}
		return nil, e.New("405: This invite link has expired.")
	}

	if err := r.DB.Clauses(clause.OnConflict{
		OnConstraint: "workspace_admins_pkey",
		DoNothing:    true,
	}).Create(&model.WorkspaceAdmin{
		AdminID:     admin.ID,
		WorkspaceID: workspace.ID,
		Role:        inviteLink.InviteeRole,
	}).Error; err != nil {
		return nil, e.Wrap(err, "500: error adding admin to association")
	}

	// Only delete the invite for specific-admin invites. Specific-admin invites are 1-time use only.
	// Non-admin specific invites are multi-use and only have an expiration date.
	if inviteLink.InviteeEmail != nil {
		if err := r.DB.Delete(inviteLink).Error; err != nil {
			return nil, e.Wrap(err, "500: error while trying to delete used invite link")
		}
	}
	return &admin.ID, nil
}

func (r *Resolver) DeleteAdminAssociation(ctx context.Context, obj interface{}, adminID int) (*int, error) {
	admin, err := r.getCurrentAdmin(ctx)
	if err != nil {
		return nil, err
	}
	if admin.ID == adminID {
		return nil, e.New("Admin tried deleting their own association")
	}

	if err := r.DB.Model(obj).Association("Admins").Delete(model.Admin{Model: model.Model{ID: adminID}}); err != nil {
		return nil, e.Wrap(err, "error deleting admin association")
	}

	return &adminID, nil
}

func (r *Resolver) isAdminInWorkspace(ctx context.Context, workspaceID int) (*model.Workspace, error) {
	span, _ := util.StartSpanFromContext(ctx, "resolver.internal.auth", util.ResourceName("isAdminInWorkspace"))
	defer span.Finish()

	span.SetAttribute("WorkspaceID", workspaceID)

	if r.isWhitelistedAccount(ctx) {
		return r.GetWorkspace(workspaceID)
	}

	admin, err := r.getCurrentAdmin(ctx)
	if err != nil {
		return nil, err
	}

	span.SetAttribute("AdminID", admin.ID)

	workspace := model.Workspace{}
	if err := r.DB.Order("name asc").
		Where(&model.Workspace{Model: model.Model{ID: workspaceID}}).
		Model(&admin).Association("Workspaces").Find(&workspace); err != nil {
		return nil, e.Wrap(err, "error getting associated workspaces")
	}

	if workspaceID != workspace.ID {
		return nil, AuthorizationError
	}

	return &workspace, nil
}

// isAdminInProject should be used for actions that you only want admins in all projects to have access to.
// Use this on actions that you don't want laymen in the demo project to have access to.
func (r *Resolver) isAdminInProject(ctx context.Context, project_id int) (*model.Project, error) {
	span, _ := util.StartSpanFromContext(ctx, "resolver.internal.auth", util.ResourceName("isAdminInProject"))
	defer span.Finish()

	span.SetAttribute("ProjectID", project_id)

	if r.isWhitelistedAccount(ctx) {
		project := &model.Project{}
		if err := r.DB.Where(&model.Project{Model: model.Model{ID: project_id}}).Take(&project).Error; err != nil {
			return nil, e.Wrap(err, "error querying project")
		}
		return project, nil
	}
	projects, err := r.Query().Projects(ctx)
	if err != nil {
		return nil, e.Wrap(err, "error querying projects")
	}
	if len(projects) == 0 {
		return nil, AuthenticationError
	}

	for _, p := range projects {
		if p.ID == project_id {
			span.SetAttribute("WorkspaceID", p.WorkspaceID)
			return p, nil
		}
	}
	return nil, AuthorizationError
}

func (r *Resolver) GetErrorGroupOccurrences(ctx context.Context, eg *model.ErrorGroup) (*time.Time, *time.Time, error) {
	bucket, measurement := r.TDB.GetSampledMeasurement(r.TDB.GetBucket(strconv.Itoa(eg.ProjectID), timeseries.Errors), timeseries.Errors, time.Since(eg.CreatedAt))
	var filter string
	if measurement == timeseries.Error.AggName {
		filter = `|> filter(fn: (r) => r._value > 0)`
	}
	query := fmt.Sprintf(`
      query = () => from(bucket: "%[1]s")
		|> range(start: 0, stop: now())
		|> filter(fn: (r) => r._measurement == "%[2]s")
		|> filter(fn: (r) => r.ErrorGroupID == "%[3]d")
    	%[4]s
		|> group(columns: ["ErrorGroupID"])

      union(tables:[query() |> first(), query() |> last()])
        |> sort(columns: ["ErrorGroupID", "_field", "_time"])
	`, bucket, measurement, eg.ID, filter)
	span, _ := util.StartSpanFromContext(ctx, "tdb.errorGroupOccurrences")
	span.SetAttribute("projectID", eg.ProjectID)
	span.SetAttribute("errorGroupID", eg.ID)
	results, err := r.TDB.Query(ctx, query)
	if err != nil {
		log.WithContext(ctx).WithError(err).Error("failed to perform tdb query for error group occurrences")
	}
	if len(results) < 2 {
		return &eg.CreatedAt, &eg.UpdatedAt, nil
	}
	return &results[0].Time, &results[1].Time, nil
}

func (r *Resolver) GetErrorGroupFrequencies(ctx context.Context, projectID int, errorGroupIDs []int, params modelInputs.ErrorGroupFrequenciesParamsInput, metric string) ([]*modelInputs.ErrorDistributionItem, error) {
	bucket, measurement := r.TDB.GetSampledMeasurement(r.TDB.GetBucket(strconv.Itoa(projectID), timeseries.Errors), timeseries.Errors, params.DateRange.EndDate.Sub(params.DateRange.StartDate))
	var errorGroupFilters []string
	for _, errorGroupID := range errorGroupIDs {
		errorGroupFilters = append(errorGroupFilters, fmt.Sprintf(`r.ErrorGroupID == "%d"`, errorGroupID))
	}
	var errorGroupFilter string
	if len(errorGroupFilters) > 0 {
		errorGroupFilter = fmt.Sprintf(`|> filter(fn: (r) => %s)`, strings.Join(errorGroupFilters, " or "))
	}
	extraFilter := ""
	if metric != "" {
		extraFilter = fmt.Sprintf(`|> filter(fn: (r) => r._field == "%s")`, metric)
	}
	query := fmt.Sprintf(`
      from(bucket: "%[1]s")
		|> range(start: %[2]s, stop: %[3]s)
		|> filter(fn: (r) => r._measurement == "%[4]s")
		%[5]s
		%[6]s
		|> aggregateWindow(every: %[7]dm, fn: sum, createEmpty: true)
		|> sort(columns: ["ErrorGroupID", "_field", "_time"])
	`, bucket, params.DateRange.StartDate.Format(time.RFC3339), params.DateRange.EndDate.Format(time.RFC3339), measurement, errorGroupFilter, extraFilter, params.ResolutionMinutes)
	span, _ := util.StartSpanFromContext(ctx, "tdb.errorGroupFrequencies")
	span.SetAttribute("projectID", projectID)
	span.SetAttribute("errorGroupIDs", errorGroupIDs)
	results, err := r.TDB.Query(ctx, query)
	if err != nil {
		log.WithContext(ctx).Error(err, "failed to perform tdb query for error group frequencies")
	}
	var response []*modelInputs.ErrorDistributionItem
	for _, r := range results {
		field := r.Values["_field"]
		if field != nil {
			var value int64
			if r.Value != nil {
				value = r.Value.(int64)
			}
			var id string
			if r.Values["ErrorGroupID"] != nil {
				id = r.Values["ErrorGroupID"].(string)
			}
			idInt, _ := strconv.Atoi(id)
			response = append(response, &modelInputs.ErrorDistributionItem{
				ErrorGroupID: idInt,
				Date:         r.Time,
				Name:         field.(string),
				Value:        value,
			})
		}
	}
	return response, nil
}

func (r *Resolver) SetErrorFrequenciesClickhouse(ctx context.Context, projectID int, errorGroups []*model.ErrorGroup, lookbackPeriod int) error {
	params := modelInputs.ErrorGroupFrequenciesParamsInput{
		DateRange: &modelInputs.DateRangeRequiredInput{
			StartDate: time.Now().Add(time.Duration(-24*lookbackPeriod) * time.Hour),
			EndDate:   time.Now(),
		},
		ResolutionMinutes: 24 * 60,
	}

	errorGroupsById := map[int]*model.ErrorGroup{}
	for _, errorGroup := range errorGroups {
		errorGroup.ErrorMetrics = []*modelInputs.ErrorDistributionItem{}
		errorGroup.ErrorFrequency = []int64{}
		errorGroupsById[errorGroup.ID] = errorGroup
	}

	frequencies, err := r.ClickhouseClient.QueryErrorGroupFrequencies(ctx, projectID, lo.Keys(errorGroupsById), params)
	if err != nil {
		return err
	}

	aggregates, err := r.ClickhouseClient.QueryErrorGroupAggregateFrequency(ctx, projectID, lo.Keys(errorGroupsById))
	if err != nil {
		return err
	}

	allMetrics := append(frequencies, aggregates...)

	for _, r := range allMetrics {
		eg := errorGroupsById[r.ErrorGroupID]
		if eg == nil {
			continue
		}
		if r.Name == "count" {
			eg.ErrorFrequency = append(eg.ErrorFrequency, r.Value)
		}
		eg.ErrorMetrics = append(eg.ErrorMetrics, &modelInputs.ErrorDistributionItem{ErrorGroupID: eg.ID, Date: r.Date, Name: r.Name, Value: r.Value})
	}

	return nil
}

func InputToParams(params *modelInputs.SearchParamsInput) *model.SearchParams {
	// Parse the inputType into the regular type.
	modelParams := &model.SearchParams{
		Browser:    params.Browser,
		OS:         params.Os,
		VisitedURL: params.VisitedURL,
		Referrer:   params.Referrer,
	}
	if params.Identified != nil {
		modelParams.Identified = *params.Identified
	}
	if params.FirstTime != nil {
		modelParams.FirstTime = *params.FirstTime
	}
	if params.HideViewed != nil {
		modelParams.HideViewed = *params.HideViewed
	}
	if params.DeviceID != nil {
		modelParams.DeviceID = params.DeviceID
	}
	if params.ShowLiveSessions != nil {
		modelParams.ShowLiveSessions = *params.ShowLiveSessions
	}
	if params.DateRange != nil {
		modelParams.DateRange = &model.DateRange{}
		if params.DateRange.StartDate != nil {
			modelParams.DateRange.StartDate = *params.DateRange.StartDate
		}
		if params.DateRange.EndDate != nil {
			modelParams.DateRange.EndDate = *params.DateRange.EndDate
		}
	}
	if params.LengthRange != nil {
		modelParams.LengthRange = &model.LengthRange{}
		if params.LengthRange.Min != nil {
			modelParams.LengthRange.Min = *params.LengthRange.Min
		}
		if params.LengthRange.Max != nil {
			modelParams.LengthRange.Max = *params.LengthRange.Max
		}
	}
	for _, property := range params.UserProperties {
		newProperty := &model.UserProperty{
			ID:    property.ID,
			Name:  property.Name,
			Value: property.Value,
		}
		modelParams.UserProperties = append(modelParams.UserProperties, newProperty)
	}
	for _, property := range params.ExcludedProperties {
		newProperty := &model.UserProperty{
			ID:    property.ID,
			Name:  property.Name,
			Value: property.Value,
		}
		modelParams.ExcludedProperties = append(modelParams.ExcludedProperties, newProperty)
	}
	for _, property := range params.TrackProperties {
		newProperty := &model.UserProperty{
			ID:    property.ID,
			Name:  property.Name,
			Value: property.Value,
		}
		modelParams.TrackProperties = append(modelParams.TrackProperties, newProperty)
	}
	modelParams.Environments = append(modelParams.Environments, params.Environments...)
	modelParams.AppVersions = append(modelParams.AppVersions, params.AppVersions...)
	modelParams.Query = params.Query
	return modelParams
}

func ErrorInputToParams(params *modelInputs.ErrorSearchParamsInput) *model.ErrorSearchParams {
	// Parse the inputType into the regular type.
	modelParams := &model.ErrorSearchParams{
		Browser:    params.Browser,
		OS:         params.Os,
		VisitedURL: params.VisitedURL,
		Event:      params.Event,
		Query:      params.Query,
	}
	if params.State != nil {
		modelParams.State = params.State
	}
	if params.DateRange != nil {
		modelParams.DateRange = &model.DateRange{}
		if params.DateRange.StartDate != nil {
			modelParams.DateRange.StartDate = *params.DateRange.StartDate
		}
		if params.DateRange.EndDate != nil {
			modelParams.DateRange.EndDate = *params.DateRange.EndDate
		}
	}
	return modelParams
}

func (r *Resolver) doesAdminOwnErrorGroup(ctx context.Context, errorGroupSecureID string) (*model.ErrorGroup, bool, error) {
	eg := &model.ErrorGroup{}

	if err := r.DB.Where(&model.ErrorGroup{SecureID: errorGroupSecureID}).Take(&eg).Error; err != nil {
		return nil, false, e.Wrap(err, "error querying error group by secureID: "+errorGroupSecureID)
	}

	_, err := r.isAdminInProjectOrDemoProject(ctx, eg.ProjectID)
	if err != nil {
		return eg, false, err
	}

	return eg, true, nil
}

func (r *Resolver) loadErrorGroupFrequenciesClickhouse(ctx context.Context, eg *model.ErrorGroup) error {
	var err error
	if eg.FirstOccurrence, eg.LastOccurrence, err = r.ClickhouseClient.QueryErrorGroupOccurrences(ctx, eg.ProjectID, eg.ID); err != nil {
		return e.Wrap(err, "error querying error group occurrences")
	}
	if err := r.SetErrorFrequenciesClickhouse(ctx, eg.ProjectID, []*model.ErrorGroup{eg}, 30); err != nil {
		return e.Wrap(err, "error querying error group frequencies")
	}
	return nil
}

func (r *Resolver) canAdminViewErrorObject(ctx context.Context, errorObjectID int) (*model.ErrorObject, error) {
	authSpan, _ := util.StartSpanFromContext(ctx, "resolver.internal.auth", util.ResourceName("canAdminViewErrorObject"))
	defer authSpan.Finish()

	errorObject := model.ErrorObject{}
	if err := r.DB.Where(&model.ErrorObject{ID: errorObjectID}).
		Preload("ErrorGroup").
		Take(&errorObject).Error; err != nil {
		return nil, err
	}

	if _, err := r.canAdminViewErrorGroup(ctx, errorObject.ErrorGroup.SecureID); err != nil {
		return nil, err
	}

	return &errorObject, nil
}

func (r *Resolver) canAdminViewErrorGroup(ctx context.Context, errorGroupSecureID string) (*model.ErrorGroup, error) {
	authSpan, _ := util.StartSpanFromContext(ctx, "resolver.internal.auth", util.ResourceName("canAdminViewErrorGroup"))
	defer authSpan.Finish()
	errorGroup, isOwner, err := r.doesAdminOwnErrorGroup(ctx, errorGroupSecureID)
	if err == nil && isOwner {
		return errorGroup, nil
	}
	if errorGroup != nil && errorGroup.IsPublic {
		return errorGroup, nil
	}
	return nil, err
}

func (r *Resolver) canAdminModifyErrorGroup(ctx context.Context, errorGroupSecureID string) (*model.ErrorGroup, error) {
	authSpan, _ := util.StartSpanFromContext(ctx, "resolver.internal.auth", util.ResourceName("canAdminModifyErrorGroup"))
	defer authSpan.Finish()
	errorGroup, isOwner, err := r.doesAdminOwnErrorGroup(ctx, errorGroupSecureID)
	if err == nil && isOwner {
		return errorGroup, nil
	}
	return nil, err
}

func (r *Resolver) _doesAdminOwnSession(ctx context.Context, sessionSecureId string) (session *model.Session, ownsSession bool, err error) {
	if session, err = r.Store.GetSessionFromSecureID(ctx, sessionSecureId); err != nil {
		return nil, false, AuthorizationError
	}
	_, err = r.isAdminInProjectOrDemoProject(ctx, session.ProjectID)
	if err != nil {
		return session, false, err
	}
	return session, true, nil
}

func (r *Resolver) canAdminViewSession(ctx context.Context, session_secure_id string) (*model.Session, error) {
	authSpan, _ := util.StartSpanFromContext(ctx, "resolver.internal.auth", util.ResourceName("canAdminViewSession"))
	defer authSpan.Finish()
	session, isOwner, err := r._doesAdminOwnSession(ctx, session_secure_id)
	if err != nil {
		if !isAuthError(err) {
			return nil, err
		} else {
			if session == nil {
				return nil, AuthorizationError
			}
			// auth error, but we should check if this is demo / public session
		}
	}
	if isOwner {
		return session, nil
	} else if session.IsPublic {
		return session, nil
	} else if session.ProjectID == r.demoProjectID(ctx) {
		return session, nil
	}
	return nil, AuthorizationError
}

func (r *Resolver) canAdminModifySession(ctx context.Context, session_secure_id string) (*model.Session, error) {
	authSpan, _ := util.StartSpanFromContext(ctx, "resolver.internal.auth", util.ResourceName("canAdminModifySession"))
	defer authSpan.Finish()
	session, isOwner, err := r._doesAdminOwnSession(ctx, session_secure_id)
	if err == nil && isOwner {
		return session, nil
	}
	return nil, err
}

func (r *Resolver) isAdminSegmentOwner(ctx context.Context, segment_id int) (*model.Segment, error) {
	authSpan, _ := util.StartSpanFromContext(ctx, "resolver.internal.auth", util.ResourceName("isAdminSegmentOwner"))
	defer authSpan.Finish()
	segment := &model.Segment{}
	if err := r.DB.Where(&model.Segment{Model: model.Model{ID: segment_id}}).Take(&segment).Error; err != nil {
		return nil, e.Wrap(err, "error querying segment")
	}
	_, err := r.isAdminInProjectOrDemoProject(ctx, segment.ProjectID)
	if err != nil {
		return nil, err
	}
	return segment, nil
}

func (r *Resolver) isAdminErrorSegmentOwner(ctx context.Context, error_segment_id int) (*model.ErrorSegment, error) {
	authSpan, _ := util.StartSpanFromContext(ctx, "resolver.internal.auth", util.ResourceName("isAdminErrorSegmentOwner"))
	defer authSpan.Finish()
	segment := &model.ErrorSegment{}
	if err := r.DB.Where(&model.ErrorSegment{Model: model.Model{ID: error_segment_id}}).Take(&segment).Error; err != nil {
		return nil, e.Wrap(err, "error querying error segment")
	}
	_, err := r.isAdminInProjectOrDemoProject(ctx, segment.ProjectID)
	if err != nil {
		return nil, err
	}
	return segment, nil
}

func (r *Resolver) SendEmailAlert(
	tos []*mail.Email,
	ccs []*mail.Email,
	authorName string,
	viewLink string,
	muteLink string,
	subjectScope string,
	textForEmail string,
	templateID string,
	sessionImage *string,
	asmGroupId *int,
) error {
	m := mail.NewV3Mail()
	from := mail.NewEmail("Highlight", Email.SendGridOutboundEmail)
	m.SetFrom(from)
	m.SetTemplateID(templateID)

	p := mail.NewPersonalization()
	p.AddTos(tos...)
	p.AddCCs(ccs...)
	p.SetDynamicTemplateData("Author_Name", authorName)
	p.SetDynamicTemplateData("Comment_Link", viewLink)
	p.SetDynamicTemplateData("Comment_Body", textForEmail)
	p.SetDynamicTemplateData("Mute_Thread", muteLink)
	p.SetDynamicTemplateData("Subject_Scope", subjectScope)

	if sessionImage != nil && *sessionImage != "" {
		p.SetDynamicTemplateData("Session_Image", sessionImage)
		a := mail.NewAttachment()
		a.SetContent(*sessionImage)
		a.SetFilename("session-image.png")
		a.SetContentID("sessionImage")
		a.SetType("image/png")
		m.AddAttachment(a)
	}

	m.AddPersonalizations(p)

	if asmGroupId != nil {
		asm := mail.NewASM()
		asm.SetGroupID(*asmGroupId)
		m.SetASM(asm)
	}

	response, err := r.MailClient.Send(m)
	if err != nil {
		return e.Wrap(err, "error sending sendgrid email for comments mentions")
	}

	if response.StatusCode == 400 {
		return e.Wrap(errors.New(response.Body), "bad request")
	}

	return nil
}

func (r *Resolver) CreateSlackBlocks(admin *model.Admin, viewLink, commentText, action string, subjectScope string, additionalContext *string) (blockSet slack.Blocks) {
	determiner := "a"
	if subjectScope == "error" {
		determiner = "an"
	}

	// Header
	message := fmt.Sprintf("You were %s in %s %s comment.", action, determiner, subjectScope)
	if admin.Email != nil && *admin.Email != "" {
		message = fmt.Sprintf("%s %s you in %s %s comment.", *admin.Email, action, determiner, subjectScope)
	}
	if admin.Name != nil && *admin.Name != "" {
		message = fmt.Sprintf("%s %s you in %s %s comment.", *admin.Name, action, determiner, subjectScope)
	}

	// comment message
	blockSet.BlockSet = append(blockSet.BlockSet, slack.NewHeaderBlock(&slack.TextBlockObject{Type: slack.PlainTextType, Text: message}))
	blockSet.BlockSet = append(blockSet.BlockSet,
		slack.NewSectionBlock(
			&slack.TextBlockObject{Type: slack.MarkdownType, Text: fmt.Sprintf("> %s", commentText)},
			nil, nil,
		),
	)

	// info on the session/error
	if subjectScope == "error" {
		blockSet.BlockSet = append(blockSet.BlockSet,
			slack.NewSectionBlock(
				&slack.TextBlockObject{Type: slack.MarkdownType, Text: fmt.Sprintf("*Error*: %s\n %s", viewLink, *additionalContext)},
				nil, nil,
			),
		)
	} else if subjectScope == "session" {
		blockSet.BlockSet = append(blockSet.BlockSet,
			slack.NewSectionBlock(
				&slack.TextBlockObject{Type: slack.MarkdownType, Text: fmt.Sprintf("*Session*: %s\n%s", viewLink, *additionalContext)},
				nil, nil,
			),
		)
	}

	button := slack.NewButtonBlockElement(
		"",
		"click",
		slack.NewTextBlockObject(
			slack.PlainTextType,
			"View comment",
			false,
			false,
		),
	)
	button.WithStyle("primary")
	button.URL = viewLink
	blockSet.BlockSet = append(blockSet.BlockSet,
		slack.NewActionBlock("action_block", button),
	)

	blockSet.BlockSet = append(blockSet.BlockSet, slack.NewDividerBlock())
	return
}

func (r *Resolver) SendSlackThreadReply(ctx context.Context, workspace *model.Workspace, admin *model.Admin, viewLink, commentText, action string, subjectScope string, threadIDs []int) error {
	if workspace.SlackAccessToken == nil {
		return nil
	}
	slackClient := slack.New(*workspace.SlackAccessToken)
	blocks := r.CreateSlackBlocks(admin, viewLink, commentText, action, subjectScope, nil)
	for _, threadID := range threadIDs {
		thread := &model.CommentSlackThread{}
		if err := r.DB.Where(&model.CommentSlackThread{Model: model.Model{ID: threadID}}).Take(&thread).Error; err != nil {
			return e.Wrap(err, "error querying slack thread")
		}
		opts := []slack.MsgOption{
			slack.MsgOptionBlocks(blocks.BlockSet...),
			slack.MsgOptionDisableLinkUnfurl(),  /** Disables showing a preview of any links that are in the Slack message.*/
			slack.MsgOptionDisableMediaUnfurl(), /** Disables showing a preview of any media that are in the Slack message.*/
			slack.MsgOptionTS(thread.ThreadTS),
		}
		_, _, err := slackClient.PostMessage(thread.SlackChannelID, opts...)
		if err != nil {
			log.WithContext(ctx).Error(err)
		}
	}
	return nil
}

func (r *Resolver) SendSlackAlertToUser(ctx context.Context, workspace *model.Workspace, admin *model.Admin, taggedSlackUsers []*modelInputs.SanitizedSlackChannelInput, viewLink, commentText, action string, subjectScope string, base64Image *string, sessionCommentID *int, errorCommentID *int, additionalContext *string) error {
	// this is needed for posting DMs
	// if nil, user simply hasn't signed up for notifications, so return nil
	if workspace.SlackAccessToken == nil {
		return nil
	}
	slackClient := slack.New(*workspace.SlackAccessToken)

	blocks := r.CreateSlackBlocks(admin, viewLink, commentText, action, subjectScope, additionalContext)

	// Prepare to upload the screenshot to the user's Slack workspace.
	// We do this instead of upload it to S3 or somewhere else to defer authorization checks to Slack.
	// If we upload the image somewhere public, anyone with the link to the image will have access. The image could contain sensitive information.
	// By uploading to the user's Slack workspace, we limit the authorization of the image to only Slack members of the user's workspace.
	var uploadedFileKey string
	if base64Image != nil {
		// This key will be used as the file name for the file written to disk.
		// This needs to be unique. The uniqueness is guaranteed by the project ID, the admin who created the comment's ID, and the current time
		uploadedFileKey = fmt.Sprintf("slack-image-%d-%d-%d.png", workspace.ID, admin.ID, time.Now().UnixNano())

		// We are writing the base64 string to disk as a png. We need to do this because the Slack Go client
		// doesn't support uploading files as base64.
		// This is something we'll need to revisit when we start getting larger traffic for comments.
		// The reason for this is each task has disk space of 20GB, each image is around 200 KB. Ideally we do everything in memory without relying on disk.
		dec, err := base64.StdEncoding.DecodeString(*base64Image)
		if err != nil {
			log.WithContext(ctx).Error(e.Wrap(err, "Failed to decode base64 image"))
		}
		f, err := os.Create(uploadedFileKey)
		if err != nil {
			log.WithContext(ctx).Error(e.Wrap(err, "Failed to create file on disk"))
		}
		defer f.Close()
		if _, err := f.Write(dec); err != nil {
			log.WithContext(ctx).Error(e.Wrap(err, "Failed to write file on disk"))
		}
		if err := f.Sync(); err != nil {
			log.WithContext(ctx).Error("Failed to sync file on disk")
		}
	}

	for _, slackUser := range taggedSlackUsers {
		if slackUser.WebhookChannelID != nil {
			_, _, _, err := slackClient.JoinConversation(*slackUser.WebhookChannelID)
			if err != nil {
				log.WithContext(ctx).Warn(e.Wrap(err, "failed to join slack channel"))
			}
			opts := []slack.MsgOption{
				slack.MsgOptionBlocks(blocks.BlockSet...),
				slack.MsgOptionDisableLinkUnfurl(),  /** Disables showing a preview of any links that are in the Slack message.*/
				slack.MsgOptionDisableMediaUnfurl(), /** Disables showing a preview of any media that are in the Slack message.*/
			}
			_, respTs, err := slackClient.PostMessage(*slackUser.WebhookChannelID, opts...)

			if err != nil {
				log.WithContext(ctx).Error(e.Wrap(err, "error posting slack message via slack bot"))
			} else {
				thread := &model.CommentSlackThread{
					SlackChannelID: *slackUser.WebhookChannelID,
					ThreadTS:       respTs,
				}
				if sessionCommentID != nil {
					thread.SessionCommentID = *sessionCommentID
				} else if errorCommentID != nil {
					thread.ErrorCommentID = *errorCommentID
				}
				r.DB.Create(thread)
			}
			if uploadedFileKey != "" {
				// We need to write the base64 image to disk, read the file, then upload it to Slack.
				// We can't send Slack a base64 string.
				fileUploadParams := slack.FileUploadParameters{
					Filetype: "image/png",
					Filename: fmt.Sprintf("Highlight %s Image.png", subjectScope),
					// These are the channels that will have access to the uploaded file.
					Channels: []string{*slackUser.WebhookChannelID},
					File:     uploadedFileKey,
					Title:    fmt.Sprintf("File from Highlight uploaded on behalf of %s", *admin.Name),
					// include the image in the initial comment's thread
					ThreadTimestamp: respTs,
				}
				_, err = slackClient.UploadFile(fileUploadParams)

				if err != nil {
					log.WithContext(ctx).Error(e.Wrap(err, "failed to upload file to Slack"))
				}
			}
		}
	}
	if uploadedFileKey != "" {
		if err := os.Remove(uploadedFileKey); err != nil {
			log.WithContext(ctx).Error(e.Wrap(err, "Failed to remove temporary session screenshot"))
		}
	}

	return nil
}

// GetSessionChunk Given a session and session-relative timestamp, finds the chunk and chunk-relative timestamp.
func (r *Resolver) GetSessionChunk(ctx context.Context, sessionID int, ts int) (chunkIdx int, chunkTs int) {
	chunkTs = ts
	var chunks []*model.EventChunk
	if err := r.DB.Order("chunk_index ASC").Model(&model.EventChunk{}).Where(&model.EventChunk{SessionID: sessionID}).
		Scan(&chunks).Error; err != nil {
		log.WithContext(ctx).Error(e.Wrap(err, "error retrieving event chunks from DB"))
		return
	}
	if len(chunks) > 1 {
		t := chunks[0].Timestamp
		absTime := t + int64(ts)
		for i, chunk := range chunks[1:] {
			if chunk.Timestamp > absTime {
				break
			}
			chunkIdx = i + 1
			t = chunk.Timestamp
		}
		chunkTs = int(absTime - t)
	}
	return
}

func (r *Resolver) getSessionInsightPrompt(ctx context.Context, events []interface{}) (string, error) {
	parsedEvents, err := parse.FilterEventsForInsights(events)
	if err != nil {
		return "", e.Wrap(err, "Failed filter session events")
	}

	b, err := json.Marshal(parsedEvents)

	if err != nil {
		return "", err
	}

	userPrompt := fmt.Sprintf(`
	Input:
	%v
	`, string(b))

	return userPrompt, nil
}

func (r *Resolver) getSessionInsight(ctx context.Context, session *model.Session) (*model.SessionInsight, error) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return nil, e.New("OPENAI_API_KEY is not set")
	}

	systemPrompt := `
	Given array of events performed by a user from a session recording for a web application, make inferences and justifications and summarize interesting things about the session in 3 insights.
	Rules:
	- Use less than 2 sentences for each point
	- Provide high level insights, do not mention singular events
	- Do not mention event types in the insights
	- Insights must be different to each other
	- Do not mention identification or authentication events
	- Don't mention timestamps in <insight>, insights should be interesting inferences from the input
	- Output timestamp that best represents the insight in <timestamp> of output
	- Sort the insights by timestamp

	You must respond ONLY with JSON that looks like this:
	[{"insight": "<Insight>", timestamp: number },{"insight": "<Insight>", timestamp: number },{"insight": "<Insight>", timestamp: number }]
	Do not provide any other output other than this format.

	Context:
	- The events are JSON objects, where the "timestamp" field represents UNIX time of the event, the "type" field represents the type of event, and the "data" field represents more information about the event
	- If the event is of type "Custom", the "tag" field provides the type of custom event, and the "payload" field represents more information about the event
	- If the event is of tag "Track", it is a custom event where the actual type of the event is in the "event" field within "data.payload"

	The "type" field follows this format:
	0 - DomContentLoaded
	1 - Load
	2 -	FullSnapshot, the full page view was loaded
	3 -	IncrementalSnapshot, some components were updated
	4 -	Meta
	5 -	Custom, the "tag" field provides the type of custom event, and the "payload" field represents more information about the event
	6 -	Plugin
	`

	events, err, _ := r.getEvents(ctx, session, model.EventsCursor{EventIndex: 0, EventObjectIndex: nil})
	if err != nil {
		log.WithContext(ctx).Error(err, "SessionInsight: GetEvents error")
		return nil, err
	}

	userPrompt, err := r.getSessionInsightPrompt(ctx, events)
	if err != nil {
		log.WithContext(ctx).Error(err, "SessionInsight: Prompt error")
		return nil, err
	}

	const MAX_AI_SESSION_INSIGHT_PROMPT_LENGTH = 32000
	if len(userPrompt) > MAX_AI_SESSION_INSIGHT_PROMPT_LENGTH {
		userPrompt = userPrompt[:MAX_AI_SESSION_INSIGHT_PROMPT_LENGTH]
	}

	client := openai.NewClient(apiKey)
	resp, err := client.CreateChatCompletion(
		context.Background(),
		openai.ChatCompletionRequest{
			Model:       openai.GPT3Dot5Turbo16K,
			Temperature: 0.7,
			Messages: []openai.ChatCompletionMessage{
				{
					Role:    openai.ChatMessageRoleSystem,
					Content: systemPrompt,
				},
				{
					Role:    openai.ChatMessageRoleUser,
					Content: userPrompt,
				},
			},
		},
	)

	if err != nil {
		log.WithContext(ctx).Error(err, "SessionInsight: ChatCompletion error")
		return nil, err
	}

	insight := &model.SessionInsight{SessionID: session.ID, Insight: resp.Choices[0].Message.Content}

	if err := r.DB.Create(insight).Error; err != nil {
		log.WithContext(ctx).Error(err, "SessionInsight: Error saving insight")
		return nil, err
	}

	return insight, nil
}

// Returns the current Admin or an Admin with ID = 0 if the current Admin is a guest
func (r *Resolver) getCurrentAdminOrGuest(ctx context.Context) (currentAdmin *model.Admin, isGuest bool) {
	admin, err := r.getCurrentAdmin(ctx)
	isGuest = false
	if admin == nil || err != nil {
		isGuest = true
		admin = &model.Admin{
			// An Admin record was created manually with an ID of 0.
			Model: model.Model{
				ID: 0,
			},
		}
	}
	return admin, isGuest
}

func (r *Resolver) SendAdminInviteImpl(adminName string, projectOrWorkspaceName string, inviteLink string, email string) (*string, error) {
	to := &mail.Email{Address: email}

	m := mail.NewV3Mail()
	from := mail.NewEmail("Highlight", Email.SendGridOutboundEmail)
	m.SetFrom(from)
	m.SetTemplateID(Email.SendAdminInviteEmailTemplateID)

	p := mail.NewPersonalization()
	p.AddTos(to)
	p.SetDynamicTemplateData("Admin_Invitor", adminName)
	p.SetDynamicTemplateData("Organization_Name", projectOrWorkspaceName)
	p.SetDynamicTemplateData("Invite_Link", inviteLink)

	m.AddPersonalizations(p)
	if resp, sendGridErr := r.MailClient.Send(m); sendGridErr != nil || resp.StatusCode >= 300 {
		estr := "error sending sendgrid email -> "
		estr += fmt.Sprintf("resp-code: %v; ", resp)
		if sendGridErr != nil {
			estr += fmt.Sprintf("err: %v", sendGridErr.Error())
		}
		return nil, e.New(estr)
	}
	return &inviteLink, nil
}

func (r *Resolver) SendWorkspaceRequestEmail(fromName string, fromEmail string, workspaceName string, toName string, toEmail string, inviteLink string) (*string, error) {
	to := &mail.Email{Address: toEmail}

	m := mail.NewV3Mail()
	from := mail.NewEmail("Highlight", Email.SendGridOutboundEmail)
	m.SetFrom(from)
	m.SetTemplateID(Email.SendGridRequestAccessEmailTemplateID)

	p := mail.NewPersonalization()
	p.AddTos(to)
	p.SetDynamicTemplateData("Requester_Name", fromName)
	p.SetDynamicTemplateData("Requester_Email", fromEmail)
	p.SetDynamicTemplateData("Workspace_Admin", toName)
	p.SetDynamicTemplateData("Workspace_Name", workspaceName)
	p.SetDynamicTemplateData("Invite_Link", inviteLink)

	m.AddPersonalizations(p)
	if resp, sendGridErr := r.MailClient.Send(m); sendGridErr != nil || resp.StatusCode >= 300 {
		estr := "error sending sendgrid email -> "
		estr += fmt.Sprintf("resp-code: %v; ", resp)
		if sendGridErr != nil {
			estr += fmt.Sprintf("err: %v", sendGridErr.Error())
		}
		return nil, e.New(estr)
	}
	return &inviteLink, nil
}

func (r *Resolver) MarshalEnvironments(environments []*string) (*string, error) {
	envBytes, err := json.Marshal(environments)
	if err != nil {
		return nil, e.Wrap(err, "error parsing environments")
	}
	envString := string(envBytes)

	return &envString, nil
}

func (r *Resolver) MarshalSlackChannelsToSanitizedSlackChannels(slackChannels []*modelInputs.SanitizedSlackChannelInput) (*string, error) {
	sanitizedChannels := []*modelInputs.SanitizedSlackChannel{}
	// For each of the new slack channels, confirm that they exist in the "IntegratedSlackChannels" string.
	for _, ch := range slackChannels {
		sanitizedChannels = append(sanitizedChannels, &modelInputs.SanitizedSlackChannel{WebhookChannel: ch.WebhookChannelName, WebhookChannelID: ch.WebhookChannelID})
	}
	channelsBytes, err := json.Marshal(sanitizedChannels)
	if err != nil {
		return nil, e.Wrap(err, "error parsing channels")
	}
	channelsString := string(channelsBytes)

	return &channelsString, nil
}

func (r *Resolver) MarshalAlertEmails(emails []*string) (*string, error) {
	emailBytes, err := json.Marshal(emails)
	if err != nil {
		return nil, e.Wrap(err, "error parsing emails")
	}
	channelsString := string(emailBytes)

	return &channelsString, nil
}

func (r *Resolver) UnmarshalStackTrace(stackTraceString string) ([]*modelInputs.ErrorTrace, error) {
	var unmarshalled []*modelInputs.ErrorTrace
	if err := json.Unmarshal([]byte(stackTraceString), &unmarshalled); err != nil {
		// Stack trace may not be able to be unmarshalled as the format may differ
		// based on the error source. This should not be treated as an error.
		return nil, nil
	}

	// Keep only non-empty stack frames
	empty := modelInputs.ErrorTrace{}
	var ret []*modelInputs.ErrorTrace
	for _, frame := range unmarshalled {
		if frame != nil && *frame != empty {
			ret = append(ret, frame)
		}
	}

	return ret, nil
}

func (r *Resolver) validateAdminRole(ctx context.Context, workspaceID int) error {
	if r.isWhitelistedAccount(ctx) {
		return nil
	}

	admin, err := r.getCurrentAdmin(ctx)
	if err != nil {
		return err
	}

	role, err := r.GetAdminRole(ctx, admin.ID, workspaceID)
	if err != nil || role != model.AdminRole.ADMIN {
		return AuthorizationError
	}

	return nil
}

// GenerateRandomBytes returns securely generated random bytes.
// It will return an error if the system's secure random
// number generator fails to function correctly, in which
// case the caller should not continue.
func GenerateRandomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := rand.Read(b)
	// Note that err == nil only if we read len(b) bytes.
	if err != nil {
		return nil, err
	}

	return b, nil
}

// GenerateRandomString returns a securely generated random string.
// It will return an error if the system's secure random
// number generator fails to function correctly, in which
// case the caller should not continue.
func GenerateRandomString(n int) (string, error) {
	const letters = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-"
	ret := make([]byte, n)
	for i := 0; i < n; i++ {
		num, err := rand.Int(rand.Reader, big.NewInt(int64(len(letters))))
		if err != nil {
			return "", err
		}
		ret[i] = letters[num.Int64()]
	}

	return string(ret), nil
}

// GenerateRandomStringURLSafe returns a URL-safe, base64 encoded
// securely generated random string.
// It will return an error if the system's secure random
// number generator fails to function correctly, in which
// case the caller should not continue.
func (r *Resolver) GenerateRandomStringURLSafe(n int) (string, error) {
	b, err := GenerateRandomBytes(n)
	return base64.URLEncoding.EncodeToString(b), err
}

func (r *Resolver) updateBillingDetails(ctx context.Context, stripeCustomerID string) error {
	customerParams := &stripe.CustomerParams{}
	customerParams.AddExpand("subscriptions")
	c, err := r.StripeClient.Customers.Get(stripeCustomerID, customerParams)
	if err != nil {
		return e.Wrapf(err, "STRIPE_INTEGRATION_ERROR error retrieving Stripe customer data for customer %s", stripeCustomerID)
	}

	subscriptions := c.Subscriptions.Data
	pricing.FillProducts(r.StripeClient, subscriptions)

	// Default to free tier
	tier := modelInputs.PlanTypeFree
	unlimitedMembers := false
	var billingPeriodStart *time.Time
	var billingPeriodEnd *time.Time
	var nextInvoiceDate *time.Time

	// Loop over each subscription item in each of the customer's subscriptions
	// and set the workspace's tier if the Stripe product has one
	for _, subscription := range subscriptions {
		for _, subscriptionItem := range subscription.Items.Data {
			if _, productTier, productUnlimitedMembers, _, _ := pricing.GetProductMetadata(subscriptionItem.Price); productTier != nil {
				tier = *productTier
				unlimitedMembers = productUnlimitedMembers
				startTimestamp := time.Unix(subscription.CurrentPeriodStart, 0)
				endTimestamp := time.Unix(subscription.CurrentPeriodEnd, 0)
				nextInvoiceTimestamp := time.Unix(subscription.NextPendingInvoiceItemInvoice, 0)

				billingPeriodStart = &startTimestamp
				billingPeriodEnd = &endTimestamp
				if subscription.NextPendingInvoiceItemInvoice != 0 {
					nextInvoiceDate = &nextInvoiceTimestamp
				}
			}
		}
	}

	workspace := model.Workspace{}
	if err := r.DB.Model(&model.Workspace{}).
		Where(model.Workspace{StripeCustomerID: &stripeCustomerID}).
		Find(&workspace).Error; err != nil {
		return e.Wrapf(err, "STRIPE_INTEGRATION_ERROR error retrieving workspace for customer %s", stripeCustomerID)
	}

	updates := map[string]interface{}{
		"PlanTier":           string(tier),
		"UnlimitedMembers":   unlimitedMembers,
		"BillingPeriodStart": billingPeriodStart,
		"BillingPeriodEnd":   billingPeriodEnd,
		"NextInvoiceDate":    nextInvoiceDate,
		"TrialEndDate":       nil,
	}

	if util.IsHubspotEnabled() {
		props := []hubspot.Property{{
			Name:     "plan_tier",
			Property: "plan_tier",
			Value:    string(tier),
		}}
		if workspace.PlanTier != modelInputs.PlanTypeFree.String() && tier == modelInputs.PlanTypeFree {
			props = append(props, hubspot.Property{
				Name:     "churn_date",
				Property: "churn_date",
				Value:    time.Now().UTC().Truncate(24 * time.Hour).UnixMilli(),
			})
		}
		if billingPeriodStart != nil {
			props = append(props, hubspot.Property{
				Name:     "billing_period_start",
				Property: "billing_period_start",
				Value:    billingPeriodStart.UTC().Truncate(24 * time.Hour).UnixMilli(),
			})
		}
		if billingPeriodEnd != nil {
			props = append(props, hubspot.Property{
				Name:     "billing_period_end",
				Property: "billing_period_end",
				Value:    billingPeriodEnd.UTC().Truncate(24 * time.Hour).UnixMilli(),
			})
		}
		if nextInvoiceDate != nil {
			props = append(props, hubspot.Property{
				Name:     "next_invoice",
				Property: "next_invoice",
				Value:    nextInvoiceDate.UTC().Truncate(24 * time.Hour).UnixMilli(),
			})
		}
		if err := r.HubspotApi.UpdateCompanyProperty(ctx, workspace.ID, props); err != nil {
			log.WithContext(ctx).WithField("props", props).Error(e.Wrap(err, "hubspot error processing stripe webhook"))
		}
	}

	if err := r.DB.Model(&model.Workspace{}).
		Where(model.Workspace{Model: model.Model{ID: workspace.ID}}).
		Updates(updates).Error; err != nil {
		return e.Wrapf(err, "STRIPE_INTEGRATION_ERROR error updating workspace fields for customer %s", stripeCustomerID)
	}

	// Plan has been updated, report the latest usage data to Stripe
	if err := pricing.NewWorker(r.DB, r.ClickhouseClient, r.StripeClient, r.MailClient, r.HubspotApi).ReportUsageForWorkspace(ctx, workspace.ID); err != nil {
		return e.Wrap(err, "STRIPE_INTEGRATION_ERROR error reporting usage after updating details")
	}

	// Make previous billing history email records inactive (so new active records can be added)
	if err := r.DB.Model(&model.BillingEmailHistory{}).
		Where(model.BillingEmailHistory{Active: true, WorkspaceID: workspace.ID}).
		Updates(map[string]interface{}{
			"Active":      false,
			"WorkspaceID": workspace.ID,
			"DeletedAt":   time.Now(),
		}).Error; err != nil {
		return e.Wrapf(err, "STRIPE_INTEGRATION_ERROR error updating BillingEmailHistory objects for workspace %d", workspace.ID)
	}

	return nil
}

func getWorkspaceIdFromUrl(parsedUrl *url.URL) (int, error) {
	pathParts := strings.Split(parsedUrl.Path, "/")
	if len(pathParts) < 2 {
		return -1, e.New("invalid url")
	}
	workspaceId, err := strconv.Atoi(pathParts[1])
	if err != nil {
		return -1, e.Wrap(err, "couldn't parse out workspace id")
	}

	return workspaceId, nil
}

func getIdForPageFromUrl(parsedUrl *url.URL, page string) (string, error) {
	pathParts := strings.Split(parsedUrl.Path, "/")
	if len(pathParts) < 4 {
		return "", e.New("invalid url")
	}
	if pathParts[2] != page || len(pathParts[3]) <= 0 {
		return "", e.New(fmt.Sprintf("url isn't for %s pages", page))
	}

	return pathParts[3], nil
}

func (r *Resolver) SlackEventsWebhook(ctx context.Context, signingSecret string) func(w http.ResponseWriter, req *http.Request) {
	return func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			log.WithContext(ctx).Error(e.Wrap(err, "couldn't read request body"))
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// verify request is from slack
		sv, err := slack.NewSecretsVerifier(req.Header, signingSecret)
		if err != nil {
			log.WithContext(ctx).Error(e.Wrap(err, "error verifying request headers"))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if _, err := sv.Write(body); err != nil {
			log.WithContext(ctx).Error(e.Wrap(err, "error when verifying request"))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if err := sv.Ensure(); err != nil {
			log.WithContext(ctx).Error(e.Wrap(err, "couldn't verify that request is from slack with the signing secret"))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		// parse events payload
		eventsAPIEvent, err := slackevents.ParseEvent(json.RawMessage(body), slackevents.OptionNoVerifyToken())
		if err != nil {
			log.WithContext(ctx).Error(e.Wrap(err, "error parsing body as a slack event"))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		if eventsAPIEvent.Type == slackevents.URLVerification {
			var r *slackevents.ChallengeResponse
			err := json.Unmarshal([]byte(body), &r)
			if err != nil {
				log.WithContext(ctx).Error(e.Wrap(err, "error parsing body as a slack challenge body"))
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text")
			if _, err := w.Write([]byte(r.Challenge)); err != nil {
				log.WithContext(ctx).Error(e.Wrap(err, "couldn't respond to slack challenge request"))
				return
			}
		}

		log.WithContext(ctx).Infof("Slack event received with event type: %s", eventsAPIEvent.InnerEvent.Type)

		if eventsAPIEvent.InnerEvent.Type == slackevents.LinkShared {
			go (func() {
				defer util.Recover()
				ev := eventsAPIEvent.InnerEvent.Data.(*slackevents.LinkSharedEvent)

				workspaceIdToSlackTeamMap := map[int]*slack.TeamInfo{}
				workspaceIdToWorkspaceMap := map[int]*model.Workspace{}
				urlToSlackAttachment := map[string]slack.Attachment{}
				var senderSlackClient *slack.Client

				for _, link := range ev.Links {
					u, err := url.Parse(link.URL)
					if err != nil {
						log.WithContext(ctx).Error(e.Wrap(err, "couldn't parse url to unfurl"))
						continue
					}

					workspaceId, err := getWorkspaceIdFromUrl(u)
					if err != nil {
						log.WithContext(ctx).Error(err)
						continue
					}

					if workspaceIdToWorkspaceMap[workspaceId] == nil {
						ws, err := r.GetWorkspace(workspaceId)
						if err != nil {
							log.WithContext(ctx).Error(e.Wrapf(err, "couldn't get workspace with workspace ID: %d (unfurl url: %s)", workspaceId, link))
							continue
						}
						workspaceIdToWorkspaceMap[workspaceId] = ws
					}

					workspace := workspaceIdToWorkspaceMap[workspaceId]

					slackAccessToken := workspace.SlackAccessToken

					if slackAccessToken == nil || len(*slackAccessToken) <= 0 {
						log.WithContext(ctx).Error(fmt.Errorf("workspace doesn't have a slack access token (unfurl url: %s)", link))
						continue
					}

					slackClient := slack.New(*slackAccessToken)

					if workspaceIdToSlackTeamMap[workspaceId] == nil {
						teamInfo, err := slackClient.GetTeamInfo()
						if err != nil {
							log.WithContext(ctx).Error(e.Wrapf(err, "couldn't get slack team information (unfurl url: %s)", link))
							continue
						}

						workspaceIdToSlackTeamMap[workspaceId] = teamInfo
					}

					if workspaceIdToSlackTeamMap[workspaceId].ID != eventsAPIEvent.TeamID {
						log.WithContext(ctx).Error(fmt.Errorf(
							"slack workspace is not authorized to view this highlight workspace (\"%s\" != \"%s\", unfurl url: %s)",
							workspaceIdToSlackTeamMap[workspaceId].ID, eventsAPIEvent.TeamID, link,
						))
						continue
					} else {
						senderSlackClient = slackClient
					}

					if sessionId, err := getIdForPageFromUrl(u, "sessions"); err == nil {
						session := model.Session{SecureID: sessionId}
						if err := r.DB.Where(&session).Take(&session).Error; err != nil {
							log.WithContext(ctx).Error(e.Wrapf(err, "couldn't get session (unfurl url: %s)", link))
							continue
						}

						attachment := slack.Attachment{}
						err = session.GetSlackAttachment(&attachment)
						if err != nil {
							log.WithContext(ctx).Error(e.Wrapf(err, "couldn't get session slack attachment (unfurl url: %s)", link))
							continue
						}
						urlToSlackAttachment[link.URL] = attachment
					} else if errorId, err := getIdForPageFromUrl(u, "errors"); err == nil {
						errorGroup := model.ErrorGroup{SecureID: errorId}
						if err := r.DB.Where(&errorGroup).Take(&errorGroup).Error; err != nil {
							log.WithContext(ctx).Error(e.Wrapf(err, "couldn't get ErrorGroup (unfurl url: %s)", link))
							continue
						}

						attachment := slack.Attachment{}
						err = errorGroup.GetSlackAttachment(&attachment)
						if err != nil {
							log.WithContext(ctx).Error(e.Wrapf(err, "couldn't get ErrorGroup slack attachment (unfurl url: %s)", link))
							continue
						}
						urlToSlackAttachment[link.URL] = attachment
					}

				}

				if len(urlToSlackAttachment) <= 0 {
					return
				}

				_, _, _, err := senderSlackClient.UnfurlMessage(ev.Channel, string(ev.MessageTimeStamp), urlToSlackAttachment)
				if err != nil {
					log.WithContext(ctx).Error(e.Wrapf(err, "failed to send slack unfurl request"))
					return
				}
			})()
		}
	}
}

const (
	projectCookieName  = "project-token"
	projectIdClaimName = "project_id"
	expClaimName       = "exp"
	projectIdUrlParam  = "project_id"
	hashValUrlParam    = "hash_val"
)

func getProjectCookieName(projectId int) string {
	return fmt.Sprintf("%s-%d", projectCookieName, projectId)
}

func getProjectIdFromToken(tokenString string) (int, error) {
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (interface{}, error) {
		return []byte(JwtAccessSecret), nil
	})
	if err != nil {
		return 0, e.Wrap(err, "invalid id token")
	}

	projectId, ok := claims[projectIdClaimName].(float64)
	if !ok {
		return 0, e.Wrap(err, "invalid project_id claim")
	}

	exp, ok := claims[expClaimName].(float64)
	if !ok {
		return 0, e.Wrap(err, "invalid exp claim")
	}

	// Check if the current time is after the expiration
	if time.Now().After(time.Unix(int64(exp), 0)) {
		return 0, e.Wrap(err, "token expired")
	}

	return int(projectId), nil
}

func (r *Resolver) ProjectJWTHandler(w http.ResponseWriter, req *http.Request) {
	projectIdStr := chi.URLParam(req, projectIdUrlParam)
	projectId, err := strconv.Atoi(projectIdStr)

	ctx := req.Context()
	if err != nil {
		log.WithContext(ctx).Error(err)
		http.Error(w, "invalid project_id", http.StatusBadRequest)
		return
	}

	_, err = r.isAdminInProjectOrDemoProject(ctx, projectId)
	if err != nil {
		log.WithContext(ctx).Error(err)
		http.Error(w, "", http.StatusForbidden)
		return
	}

	atClaims := jwt.MapClaims{}
	atClaims[projectIdClaimName] = projectId
	atClaims[expClaimName] = time.Now().Add(time.Hour).Unix()
	at := jwt.NewWithClaims(jwt.SigningMethodHS256, atClaims)
	token, err := at.SignedString([]byte(JwtAccessSecret))
	if err != nil {
		log.WithContext(ctx).Error(err)
		http.Error(w, "", http.StatusInternalServerError)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     getProjectCookieName(projectId),
		Value:    token,
		MaxAge:   int(time.Hour.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteNoneMode,
		Path:     "/",
	})
	w.WriteHeader(http.StatusOK)
}

func (r *Resolver) AssetHandler(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	projectIdParam := chi.URLParam(req, projectIdUrlParam)
	hashValParam := chi.URLParam(req, hashValUrlParam)

	projectId, err := strconv.Atoi(projectIdParam)
	if err != nil {
		log.WithContext(ctx).Error(e.Wrap(err, "error converting project_id param to string"))
		http.Error(w, "", http.StatusBadRequest)
		return
	}

	projectCookie, err := req.Cookie(getProjectCookieName(projectId))
	if err != nil {
		log.WithContext(ctx).Error(e.Wrap(err, "error accessing projectToken cookie"))
		http.Error(w, "", http.StatusForbidden)
		return
	}

	projectIdFromToken, err := getProjectIdFromToken(projectCookie.Value)
	if err != nil {
		log.WithContext(ctx).Error(e.Wrap(err, "error getting project id from token claims"))
		http.Error(w, "", http.StatusForbidden)
		return
	}

	if projectIdFromToken != projectId {
		log.WithContext(ctx).Error(e.Wrap(err, "project id mismatch"))
		w.WriteHeader(http.StatusForbidden)
		return
	}

	url, err := r.StorageClient.GetAssetURL(ctx, projectIdParam, hashValParam)
	if err != nil {
		log.WithContext(ctx).Error(e.Wrap(err, "failed to generate asset url"))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Cache the redirected url for up to 14 minutes
	w.Header().Set("Cache-Control", "max-age=840")
	http.Redirect(w, req, url, http.StatusFound)
}

func (r *Resolver) StripeWebhook(ctx context.Context, endpointSecret string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, req *http.Request) {
		const MaxBodyBytes = int64(65536)
		req.Body = http.MaxBytesReader(w, req.Body, MaxBodyBytes)
		payload, err := io.ReadAll(req.Body)
		if err != nil {
			log.WithContext(ctx).Error(e.Wrap(err, "error reading request body"))
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		event, err := webhook.ConstructEvent(payload, req.Header.Get("Stripe-Signature"),
			endpointSecret)
		if err != nil {
			log.WithContext(ctx).Error(e.Wrap(err, "error verifying webhook signature"))
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		if err := json.Unmarshal(payload, &event); err != nil {
			log.WithContext(ctx).Error(e.Wrap(err, "failed to parse webhook body json"))
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		log.WithContext(ctx).Infof("Stripe webhook received event type: %s", event.Type)

		switch event.Type {
		case "customer.subscription.created", "customer.subscription.updated", "customer.subscription.deleted":
			var subscription stripe.Subscription
			err := json.Unmarshal(event.Data.Raw, &subscription)
			if err != nil {
				log.WithContext(ctx).Error(e.Wrap(err, "failed to parse webhook body json as Subscription"))
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			if err := r.updateBillingDetails(ctx, subscription.Customer.ID); err != nil {
				log.WithContext(ctx).Error(e.Wrap(err, "failed to update billing details"))
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}

		w.WriteHeader(http.StatusOK)
	}
}

func (r *Resolver) CreateInviteLink(workspaceID int, email *string, role string, shouldExpire bool) *model.WorkspaceInviteLink {
	// Unit is days.
	EXPIRATION_DATE := 30
	expirationDate := time.Now().UTC().AddDate(0, 0, EXPIRATION_DATE)

	secret, _ := r.GenerateRandomStringURLSafe(16)

	newInviteLink := &model.WorkspaceInviteLink{
		WorkspaceID:    &workspaceID,
		InviteeEmail:   email,
		InviteeRole:    &role,
		ExpirationDate: &expirationDate,
		Secret:         &secret,
	}

	if !shouldExpire {
		newInviteLink.ExpirationDate = nil
	}

	return newInviteLink
}

func (r *Resolver) AddFrontToProject(ctx context.Context, project *model.Project, code string) error {
	oauth, err := front.OAuth(ctx, code, nil)
	if err != nil {
		return e.Wrapf(err, "failed to add front to project id %d", project.ID)
	}

	return r.saveFrontOAuth(project, oauth)
}

func (r *Resolver) AddVercelToWorkspace(workspace *model.Workspace, code string) error {
	res, err := vercel.GetAccessToken(code)
	if err != nil {
		return e.Wrap(err, "error getting Vercel oauth access token")
	}

	if err := r.DB.Where(&workspace).Select("vercel_access_token", "vercel_team_id").Updates(&model.Workspace{VercelAccessToken: &res.AccessToken, VercelTeamID: res.TeamID}).Error; err != nil {
		return e.Wrap(err, "error updating Vercel access token in workspace")
	}

	return nil
}

func (r *Resolver) AddClickUpToWorkspace(ctx context.Context, workspace *model.Workspace, code string) error {
	res, err := clickup.GetAccessToken(ctx, code)
	if err != nil {
		return e.Wrap(err, "error getting ClickUp oauth access token")
	}

	if err := r.DB.Where(&workspace).Select("clickup_access_token").Updates(&model.Workspace{ClickupAccessToken: &res.AccessToken}).Error; err != nil {
		return e.Wrap(err, "error updating ClickUp access token in workspace")
	}

	return nil
}

func (r *Resolver) AddHeightToWorkspace(ctx context.Context, workspace *model.Workspace, code string) error {
	return r.IntegrationsClient.GetAndSetWorkspaceToken(ctx, workspace, modelInputs.IntegrationTypeHeight, code)
}

func (r *Resolver) AddGitHubToWorkspace(ctx context.Context, workspace *model.Workspace, code string) error {
	// a bit of a hack here, but the `code` is actually the `integrationID` which is provided
	// from github after installation via a callback. this allows us to save the
	// installation of this app for authenticating.
	integrationWorkspaceMapping := &model.IntegrationWorkspaceMapping{
		WorkspaceID:     workspace.ID,
		IntegrationType: modelInputs.IntegrationTypeGitHub,
		AccessToken:     code,
	}

	if err := r.DB.Clauses(clause.OnConflict{
		UpdateAll: true,
	}).Create(integrationWorkspaceMapping).Error; err != nil {
		return err
	}
	return nil
}

func (r *Resolver) AddDiscordToWorkspace(ctx context.Context, workspace *model.Workspace, code string) error {
	token, err := discord.OAuth(ctx, code)

	if err != nil {
		return e.Wrapf(err, "failed to get oauth token when connecting discord to workspace id %d", workspace.ID)
	}

	guild := token.Extra("guild").(map[string]interface{})
	guildId := guild["id"].(string)

	if guildId == "" {
		return e.Wrapf(err, "failed to extra guild id from discord oauth response")
	}

	if err := r.DB.Where(&workspace).Updates(&model.Workspace{DiscordGuildId: &guildId}).Error; err != nil {
		return e.Wrap(err, "error updating discord guild id on workspace")
	}

	return nil
}

func (r *Resolver) saveFrontOAuth(project *model.Project, oauth *front.OAuthToken) error {
	exp := time.Unix(oauth.ExpiresAt, 0)
	if err := r.DB.Where(&project).Updates(&model.Project{FrontAccessToken: &oauth.AccessToken,
		FrontRefreshToken: &oauth.RefreshToken, FrontTokenExpiresAt: &exp}).Error; err != nil {
		return e.Wrap(err, "error updating front access token on project")
	}
	return nil
}

func (r *Resolver) AddSlackToWorkspace(ctx context.Context, workspace *model.Workspace, code string) error {
	var (
		SLACK_CLIENT_ID     string
		SLACK_CLIENT_SECRET string
	)

	if tempSlackClientID, ok := os.LookupEnv("SLACK_CLIENT_ID"); ok && tempSlackClientID != "" {
		SLACK_CLIENT_ID = tempSlackClientID
	}
	if tempSlackClientSecret, ok := os.LookupEnv("SLACK_CLIENT_SECRET"); ok && tempSlackClientSecret != "" {
		SLACK_CLIENT_SECRET = tempSlackClientSecret
	}

	redirect := FrontendURI + "/callback/slack"

	resp, err := slack.
		GetOAuthV2Response(
			&http.Client{},
			SLACK_CLIENT_ID,
			SLACK_CLIENT_SECRET,
			code,
			redirect,
		)

	if err != nil {
		return e.Wrap(err, "error getting slack oauth response")
	}

	if err := r.DB.Where(&workspace).Updates(&model.Workspace{SlackAccessToken: &resp.AccessToken}).Error; err != nil {
		return e.Wrap(err, "error updating slack access token in workspace")
	}

	existingChannels, _, _ := r.GetSlackChannelsFromSlack(ctx, workspace.ID)
	channelBytes, err := json.Marshal(existingChannels)
	if err != nil {
		return e.Wrap(err, "error marshaling existing channels")
	}

	channelString := string(channelBytes)
	if err := r.DB.Model(&workspace).Updates(&model.Workspace{
		SlackChannels: &channelString,
	}).Error; err != nil {
		return e.Wrap(err, "error updating project fields")
	}

	return nil
}

func (r *Resolver) RemoveSlackFromWorkspace(workspace *model.Workspace, projectID int) error {
	if err := r.DB.Transaction(func(tx *gorm.DB) error {
		// remove slack integration from workspace
		if err := tx.Where(&workspace).Select("slack_access_token", "slack_channels").Updates(&model.Workspace{SlackAccessToken: nil, SlackChannels: nil}).Error; err != nil {
			return e.Wrap(err, "error removing slack access token and channels in workspace")
		}

		empty := "[]"
		projectAlert := model.Alert{ProjectID: projectID}
		clearedChannelsAlert := model.Alert{ChannelsToNotify: &empty}

		// set existing alerts to have empty slack channels to notify
		if err := tx.Where(&model.SessionAlert{Alert: projectAlert}).Updates(model.SessionAlert{Alert: clearedChannelsAlert}).Error; err != nil {
			return e.Wrap(err, "error removing slack channels from created SessionAlert's")
		}

		if err := tx.Where(&model.ErrorAlert{Alert: projectAlert}).Updates(model.ErrorAlert{Alert: clearedChannelsAlert}).Error; err != nil {
			return e.Wrap(err, "error removing slack channels from created ErrorAlert's")
		}

		// set existing metric monitors to have empty slack channels to notify
		if err := tx.Where(&model.MetricMonitor{ProjectID: projectID}).Updates(model.MetricMonitor{ChannelsToNotify: &empty}).Error; err != nil {
			return e.Wrap(err, "error removing slack channels from created MetricMonitor's")
		}

		if err := tx.Where(&model.LogAlert{Alert: projectAlert}).Updates(model.LogAlert{Alert: clearedChannelsAlert}).Error; err != nil {
			return e.Wrap(err, "error removing slack channels from created LogAlert's")
		}

		// no errors updating DB
		return nil
	}); err != nil {
		return err
	}

	return nil
}

func (r *Resolver) RemoveZapierFromWorkspace(project *model.Project) error {
	if err := r.DB.Where(&project).Select("zapier_access_token").Updates(&model.Project{ZapierAccessToken: nil}).Error; err != nil {
		return e.Wrap(err, "error removing zapier access token in project model")
	}

	return nil
}

func (r *Resolver) RemoveFrontFromProject(project *model.Project) error {
	if err := r.DB.Where(&project).Select("front_access_token").Updates(&model.Project{FrontAccessToken: nil}).Error; err != nil {
		return e.Wrap(err, "error removing front access token in project model")
	}

	return nil
}

func (r *Resolver) RemoveVercelFromWorkspace(workspace *model.Workspace) error {
	if workspace.VercelAccessToken == nil {
		return e.New("workspace does not have a Vercel access token")
	}

	projects, err := vercel.GetProjects(*workspace.VercelAccessToken, workspace.VercelTeamID)
	if err != nil {
		return err
	}

	configIdsToRemove := map[string]struct{}{}
	for _, p := range projects {
		for _, e := range p.Env {
			if e.Key == vercel.SourcemapEnvKey {
				if e.ConfigurationID == "" {
					continue
				}
				configIdsToRemove[e.ConfigurationID] = struct{}{}
			}
		}
	}

	for c := range configIdsToRemove {
		if err := vercel.RemoveConfiguration(c, *workspace.VercelAccessToken, workspace.VercelTeamID); err != nil {
			return err
		}
	}

	if err := r.DB.Where(workspace).
		Select("vercel_access_token", "vercel_team_id").
		Updates(&model.Workspace{VercelAccessToken: nil, VercelTeamID: nil}).Error; err != nil {
		return e.Wrap(err, "error removing Vercel access token and team id")
	}

	return nil
}

func (r *Resolver) RemoveClickUpFromWorkspace(workspace *model.Workspace) error {
	if workspace.ClickupAccessToken == nil {
		return e.New("workspace does not have a ClickUp access token")
	}

	if err := r.DB.Raw(`
		DELETE FROM integration_project_mappings ipm
		WHERE ipm.integration_type = ?
		AND EXISTS (
			SELECT *
			FROM projects p
			WHERE p.workspace_id = ?
			AND ipm.project_id = p.id
		)
	`, modelInputs.IntegrationTypeClickUp, workspace.ID).Error; err != nil {
		return err
	}

	if err := r.DB.Where(workspace).
		Select("clickup_access_token").
		Updates(&model.Workspace{ClickupAccessToken: nil}).Error; err != nil {
		return e.Wrap(err, "error removing ClickUp access token")
	}

	return nil
}

func (r *Resolver) RemoveGitHubFromWorkspace(ctx context.Context, workspace *model.Workspace) error {
	workspaceMapping := &model.IntegrationWorkspaceMapping{}
	if err := r.DB.Where(&model.IntegrationWorkspaceMapping{
		WorkspaceID:     workspace.ID,
		IntegrationType: modelInputs.IntegrationTypeGitHub,
	}).Take(&workspaceMapping).Error; err != nil {
		return e.Wrap(err, "workspace does not have a GitHub integration")
	}

	// uninstall the app in github
	if c, err := github.NewClient(ctx, workspaceMapping.AccessToken, r.Redis); err == nil {
		if err := c.DeleteInstallation(ctx, workspaceMapping.AccessToken); err != nil {
			return e.Wrap(err, "failed to delete github app installation")
		}
	} else {
		return e.Wrap(err, "failed to create github client")
	}

	if err := r.DB.Exec(`
		UPDATE services
		SET github_repo_path = NULL, github_prefix = NULL, build_prefix = NULL, status = 'created', error_details = ARRAY[]::text[]
		FROM projects
		WHERE projects.workspace_id = ?
			AND services.project_id = projects.id
	`, workspace.ID).Error; err != nil {
		return e.Wrap(err, "failed to remove GitHub repo from associated project services")
	}

	if err := r.DB.Delete(workspaceMapping).Error; err != nil {
		return e.Wrap(err, "error deleting workspace GitHub integration")
	}

	return nil
}

func (r *Resolver) RemoveIntegrationFromWorkspaceAndProjects(ctx context.Context, workspace *model.Workspace, integrationType modelInputs.IntegrationType) error {
	workspaceMapping := &model.IntegrationWorkspaceMapping{}
	if err := r.DB.Where(&model.IntegrationWorkspaceMapping{
		WorkspaceID:     workspace.ID,
		IntegrationType: integrationType,
	}).Take(&workspaceMapping).Error; err != nil {
		return e.Wrap(err, fmt.Sprintf("workspace does not have a %s integration", integrationType))
	}

	if err := r.DB.Raw(`
		DELETE FROM integration_project_mappings ipm
		WHERE ipm.integration_type = ?
		AND EXISTS (
			SELECT *
			FROM projects p
			WHERE p.workspace_id = ?
			AND ipm.project_id = p.id
		)
	`, integrationType, workspace.ID).Error; err != nil {
		return err
	}

	if err := r.DB.Delete(workspaceMapping).Error; err != nil {
		return e.Wrap(err, fmt.Sprintf("error deleting workspace %s integration", integrationType))
	}

	return nil
}

func (r *Resolver) RemoveDiscordFromWorkspace(workspace *model.Workspace) error {
	if err := r.DB.Where(&workspace).Select("discord_guild_id").Updates(&model.Workspace{DiscordGuildId: nil}).Error; err != nil {
		return e.Wrap(err, "error removing discord guild id from workspace model")
	}

	return nil
}

func (r *Resolver) AddLinearToWorkspace(workspace *model.Workspace, code string) error {
	var (
		LINEAR_CLIENT_ID     string
		LINEAR_CLIENT_SECRET string
	)

	if tempLinearClientID, ok := os.LookupEnv("LINEAR_CLIENT_ID"); ok && tempLinearClientID != "" {
		LINEAR_CLIENT_ID = tempLinearClientID
	}
	if tempLinearClientSecret, ok := os.LookupEnv("LINEAR_CLIENT_SECRET"); ok && tempLinearClientSecret != "" {
		LINEAR_CLIENT_SECRET = tempLinearClientSecret
	}

	redirect := FrontendURI + "/callback/linear"

	res, err := r.GetLinearAccessToken(code, redirect, LINEAR_CLIENT_ID, LINEAR_CLIENT_SECRET)
	if err != nil {
		return e.Wrap(err, "error getting linear oauth access token")
	}

	if err := r.DB.Where(&workspace).Updates(&model.Workspace{LinearAccessToken: &res.AccessToken}).Error; err != nil {
		return e.Wrap(err, "error updating slack access token in workspace")
	}

	return nil
}

func (r *Resolver) RemoveLinearFromWorkspace(workspace *model.Workspace) error {
	if err := r.RevokeLinearAccessToken(*workspace.LinearAccessToken); err != nil {
		return err
	}

	if err := r.DB.Where(&workspace).Select("linear_access_token").Updates(&model.Workspace{LinearAccessToken: nil}).Error; err != nil {
		return e.Wrap(err, "error removing linear access token in workspace")
	}

	return nil
}

func (r *Resolver) MakeLinearGraphQLRequest(accessToken string, body string) ([]byte, error) {
	client := &http.Client{}

	req, err := http.NewRequest("POST", "https://api.linear.app/graphql", strings.NewReader(body))
	if err != nil {
		return nil, e.Wrap(err, "error creating api request to linear")
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", accessToken))
	req.Header.Set("Content-Type", "application/json")

	res, err := client.Do(req)
	if err != nil {
		return nil, e.Wrap(err, "error getting response from linear graphql endpoint")
	}

	b, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, e.Wrap(err, "error reading response body from linear graphql endpoint")
	}

	if res.StatusCode != 200 {
		return nil, e.New("linear graphql API responded with error; status_code=" + res.Status + "; body=" + string(b))
	}

	return b, nil
}

type LinearTeam struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Key  string `json:"key"`
}
type LinearTeamsResponse struct {
	Data struct {
		Teams struct {
			Nodes []LinearTeam `json:"nodes"`
		} `json:"teams"`
	} `json:"data"`
}

func (r *Resolver) GetLinearTeams(accessToken string) (*LinearTeamsResponse, error) {
	requestQuery := `
	query {
		teams {
			nodes {
				id
				name
				key
			}
		}
	}
	`

	type GraphQLReq struct {
		Query string `json:"query"`
	}

	req := GraphQLReq{Query: requestQuery}
	requestBytes, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	b, err := r.MakeLinearGraphQLRequest(accessToken, string(requestBytes))
	if err != nil {
		return nil, err
	}

	teamsResponse := &LinearTeamsResponse{}

	err = json.Unmarshal(b, teamsResponse)
	if err != nil {
		return nil, e.Wrap(err, "error unmarshaling linear oauth token response")
	}

	return teamsResponse, nil
}

type LinearCreateIssueResponse struct {
	Data struct {
		IssueCreate struct {
			Issue struct {
				ID         string `json:"id"`
				Identifier string `json:"identifier"`
			} `json:"issue"`
		} `json:"issueCreate"`
	} `json:"data"`
}

func (r *Resolver) CreateLinearIssue(accessToken string, teamID string, title string, description string) (*LinearCreateIssueResponse, error) {
	requestQuery := `
	mutation createIssue($teamId: String!, $title: String!, $desc: String!) {
		issueCreate(input: {teamId: $teamId, title: $title, description: $desc}) {
			issue {
				id,
				identifier
			}
		}
	}
	`

	type GraphQLVars struct {
		TeamID string `json:"teamId"`
		Title  string `json:"title"`
		Desc   string `json:"desc"`
	}

	type GraphQLReq struct {
		Query     string      `json:"query"`
		Variables GraphQLVars `json:"variables"`
	}

	req := GraphQLReq{Query: requestQuery, Variables: GraphQLVars{TeamID: teamID, Title: title, Desc: description}}

	requestBytes, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	b, err := r.MakeLinearGraphQLRequest(accessToken, string(requestBytes))
	if err != nil {
		return nil, err
	}

	createIssueRes := &LinearCreateIssueResponse{}

	err = json.Unmarshal(b, createIssueRes)
	if err != nil {
		return nil, e.Wrap(err, "error unmarshaling linear oauth token response")
	}

	return createIssueRes, nil
}

type LinearCreateAttachmentResponse struct {
	Data struct {
		AttachmentCreate struct {
			Attachment struct {
				ID string `json:"id"`
			} `json:"Attachment"`
			Success bool `json:"success"`
		} `json:"attachmentCreate"`
	} `json:"data"`
}

func (r *Resolver) CreateLinearAttachment(accessToken string, issueID string, title string, subtitle string, url string) (*LinearCreateAttachmentResponse, error) {
	requestQuery := `
	mutation createAttachment($issueId: String!, $url: String!, $iconUrl: String!, $title: String!, $subtitle: String) {
		attachmentCreate(input: {issueId: $issueId, url: $url, iconUrl: $iconUrl, title: $title, subtitle: $subtitle}) {
		  attachment {
			id
		  }
		  success
		}
	  }
	`

	type GraphQLVars struct {
		IssueID  string `json:"issueId"`
		Title    string `json:"title"`
		Subtitle string `json:"subtitle"`
		Url      string `json:"url"`
		IconUrl  string `json:"iconUrl"`
	}

	type GraphQLReq struct {
		Query     string      `json:"query"`
		Variables GraphQLVars `json:"variables"`
	}

	req := GraphQLReq{
		Query: requestQuery,
		Variables: GraphQLVars{
			IssueID:  issueID,
			Title:    title,
			Subtitle: subtitle,
			Url:      url,
			IconUrl:  fmt.Sprintf("%s/logo_with_gradient_bg.png", os.Getenv("FRONTEND_URI")),
		},
	}

	requestBytes, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	b, err := r.MakeLinearGraphQLRequest(accessToken, string(requestBytes))
	if err != nil {
		return nil, err
	}

	createAttachmentRes := &LinearCreateAttachmentResponse{}

	err = json.Unmarshal(b, createAttachmentRes)
	if err != nil {
		return nil, e.Wrap(err, "error unmarshaling linear oauth token response")
	}

	if !createAttachmentRes.Data.AttachmentCreate.Success {
		return nil, e.New("failed to create linear attachment")
	}

	return createAttachmentRes, nil
}

func (r *Resolver) CreateLinearIssueAndAttachment(
	workspace *model.Workspace,
	attachment *model.ExternalAttachment,
	issueTitle string,
	issueDescription string,
	commentText string,
	authorName string,
	viewLink string,
	teamId *string,
) error {
	if teamId == nil {
		teamRes, err := r.GetLinearTeams(*workspace.LinearAccessToken)
		if err != nil {
			return err
		}

		if len(teamRes.Data.Teams.Nodes) <= 0 {
			return e.New("no teams to make a linear issue to")
		}

		teamId = &teamRes.Data.Teams.Nodes[0].ID
	}

	issueRes, err := r.CreateLinearIssue(*workspace.LinearAccessToken, *teamId, issueTitle, issueDescription)
	if err != nil {
		return err
	}

	attachmentRes, err := r.CreateLinearAttachment(*workspace.LinearAccessToken, issueRes.Data.IssueCreate.Issue.ID, commentText, authorName, viewLink)
	if err != nil {
		return err
	}

	attachment.ExternalID = attachmentRes.Data.AttachmentCreate.Attachment.ID
	attachment.Title = issueRes.Data.IssueCreate.Issue.Identifier

	if err := r.DB.Create(attachment).Error; err != nil {
		return e.Wrap(err, "error creating external attachment")
	}
	return nil
}

func (r *Resolver) CreateClickUpTaskAndAttachment(
	workspace *model.Workspace,
	attachment *model.ExternalAttachment,
	issueTitle string,
	issueDescription string,
	teamId *string,
) error {
	// raise an error if the team id is not set
	if teamId == nil {
		return e.New("illegal argument: listId is nil")
	}

	task, err := clickup.CreateTask(*workspace.ClickupAccessToken, *teamId, issueTitle, issueDescription)
	if err != nil {
		return err
	}

	attachment.ExternalID = task.ID
	attachment.Title = task.Name
	if err := r.DB.Create(attachment).Error; err != nil {
		return e.Wrap(err, "error creating external attachment")
	}
	return nil
}

func (r *Resolver) CreateHeightTaskAndAttachment(
	ctx context.Context,
	workspace *model.Workspace,
	attachment *model.ExternalAttachment,
	issueTitle string,
	issueDescription string,
	teamId *string,
) error {
	accessToken, err := r.IntegrationsClient.GetWorkspaceAccessToken(ctx, workspace, modelInputs.IntegrationTypeHeight)

	if err != nil {
		return err
	}

	if accessToken == nil {
		return errors.New("No Height integration access token found.")
	}
	task, err := height.CreateTask(*accessToken, *teamId, issueTitle, issueDescription)
	if err != nil {
		return err
	}

	attachment.ExternalID = task.ID
	attachment.Title = task.Name
	if err := r.DB.Create(attachment).Error; err != nil {
		return e.Wrap(err, "error creating external attachment")
	}
	return nil
}

func (r *Resolver) CreateGitHubTaskAndAttachment(
	ctx context.Context,
	workspace *model.Workspace,
	attachment *model.ExternalAttachment,
	issueTitle string,
	issueDescription string,
	repo *string,
	tags []*modelInputs.SessionCommentTagInput,
) error {
	labels := lo.Map(tags, func(t *modelInputs.SessionCommentTagInput, i int) string {
		return t.Name
	})

	accessToken, err := r.IntegrationsClient.GetWorkspaceAccessToken(ctx, workspace, modelInputs.IntegrationTypeGitHub)

	if err != nil {
		return err
	}

	if accessToken == nil {
		return errors.New("No GitHub integration access token found.")
	}
	var task *github2.Issue
	if c, err := github.NewClient(ctx, *accessToken, r.Redis); err == nil {
		task, err = c.CreateIssue(ctx, *repo, &github2.IssueRequest{
			Title:  pointy.String(issueTitle),
			Body:   pointy.String(issueDescription),
			Labels: &labels,
		})
		if err != nil {
			return err
		}
	} else {
		return e.Wrap(err, "failed to create github client")
	}

	attachment.ExternalID = task.GetHTMLURL()
	attachment.Title = task.GetTitle()
	if err := r.DB.Create(attachment).Error; err != nil {
		return e.Wrap(err, "error creating external attachment")
	}
	return nil
}

func (r *Resolver) GetGitHubRepos(
	ctx context.Context,
	workspace *model.Workspace,
) ([]*modelInputs.GitHubRepo, error) {
	accessToken, err := r.IntegrationsClient.GetWorkspaceAccessToken(ctx, workspace, modelInputs.IntegrationTypeGitHub)
	if err != nil {
		return nil, err
	}

	if accessToken == nil {
		return nil, nil
	}
	var repos []*github2.Repository
	if c, err := github.NewClient(ctx, *accessToken, r.Redis); err == nil {
		repos, err = c.ListRepos(ctx)
		if err != nil {
			return nil, err
		}
	} else {
		return nil, e.Wrap(err, "failed to create github client")
	}

	return lo.Map(repos, func(t *github2.Repository, i int) *modelInputs.GitHubRepo {
		return &modelInputs.GitHubRepo{
			RepoID: t.GetURL(),
			Name:   t.GetName(),
			Key:    strconv.FormatInt(t.GetID(), 10),
		}
	}), nil
}

func (r *Resolver) GetGitHubIssueLabels(
	ctx context.Context,
	workspace *model.Workspace,
	repository string,
) ([]string, error) {
	accessToken, err := r.IntegrationsClient.GetWorkspaceAccessToken(ctx, workspace, modelInputs.IntegrationTypeGitHub)
	if err != nil {
		return nil, err
	}

	if accessToken == nil {
		return nil, nil
	}
	var labels []*github2.Label
	if c, err := github.NewClient(ctx, *accessToken, r.Redis); err == nil {
		labels, err = c.ListLabels(ctx, repository)
		if err != nil {
			return nil, err
		}
	} else {
		return nil, e.Wrap(err, "failed to create github client")
	}

	return lo.Map(labels, func(l *github2.Label, i int) string {
		return l.GetName()
	}), nil
}

type LinearAccessTokenResponse struct {
	AccessToken string   `json:"access_token"`
	TokenType   string   `json:"token_type"`
	ExpiresIn   int64    `json:"expires_in"`
	Scope       []string `json:"scope"`
}

func (r *Resolver) GetLinearAccessToken(code string, redirectURL string, clientID string, clientSecret string) (LinearAccessTokenResponse, error) {
	client := &http.Client{}

	data := url.Values{}
	data.Set("code", code)
	data.Set("client_id", clientID)
	data.Set("client_secret", clientSecret)
	data.Set("grant_type", "authorization_code")
	data.Set("redirect_uri", redirectURL)

	accessTokenResponse := LinearAccessTokenResponse{}

	req, err := http.NewRequest("POST", "https://api.linear.app/oauth/token", strings.NewReader(data.Encode()))
	if err != nil {
		return accessTokenResponse, e.Wrap(err, "error creating api request to linear")
	}
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	res, err := client.Do(req)

	if err != nil {
		return accessTokenResponse, e.Wrap(err, "error getting response from linear oauth token endpoint")
	}

	b, err := io.ReadAll(res.Body)

	if res.StatusCode != 200 {
		return accessTokenResponse, e.New("linear API responded with error; status_code=" + res.Status + "; body=" + string(b))
	}

	if err != nil {
		return accessTokenResponse, e.Wrap(err, "error reading response body from linear oauth token endpoint")
	}
	err = json.Unmarshal(b, &accessTokenResponse)
	if err != nil {
		return accessTokenResponse, e.Wrap(err, "error unmarshaling linear oauth token response")
	}

	return accessTokenResponse, nil
}

func (r *Resolver) RevokeLinearAccessToken(accessToken string) error {
	client := &http.Client{}

	data := url.Values{}
	data.Set("access_token", accessToken)

	req, err := http.NewRequest("POST", "https://api.linear.app/oauth/revoke", strings.NewReader(data.Encode()))
	if err != nil {
		return e.Wrap(err, "error creating api request to linear")
	}

	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	res, err := client.Do(req)
	if err != nil {
		return e.Wrap(err, "error getting response from linear revoke oauth token endpoint")
	}

	if res.StatusCode != 200 {
		return e.New("linear API responded with error; status_code=" + res.Status)
	}

	return nil
}

func (r *Resolver) getCommentFollowers(ctx context.Context, followers []*model.CommentFollower) (existingAdmins []int, existingSlackChannelIDs []string) {
	for _, f := range followers {
		if f.AdminId > 0 {
			existingAdmins = append(existingAdmins, f.AdminId)
		} else if len(f.SlackChannelID) > 0 {
			existingSlackChannelIDs = append(existingSlackChannelIDs, f.SlackChannelID)
		}
	}
	return
}

func (r *Resolver) findNewFollowers(taggedAdmins []*modelInputs.SanitizedAdminInput, taggedSlackUsers []*modelInputs.SanitizedSlackChannelInput, existingAdminIDs []int, existingSlackChannelIDs []string) (newFollowers []*model.CommentFollower) {
	for _, a := range taggedAdmins {
		exists := false
		for _, id := range existingAdminIDs {
			if id == a.ID {
				exists = true
				break
			}
		}
		if a.ID > 0 && !exists {
			newCommentFollow := model.CommentFollower{
				AdminId: a.ID,
			}
			newFollowers = append(newFollowers, &newCommentFollow)
		}
	}
	for _, s := range taggedSlackUsers {
		exists := false
		for _, id := range existingSlackChannelIDs {
			if id == *s.WebhookChannelID {
				exists = true
				break
			}
		}
		if len(*s.WebhookChannelID) > 0 && !exists {
			newCommentFollow := model.CommentFollower{
				SlackChannelName: *s.WebhookChannelName,
				SlackChannelID:   *s.WebhookChannelID,
			}
			newFollowers = append(newFollowers, &newCommentFollow)
		}
	}
	return
}

func (r *Resolver) sendFollowedCommentNotification(
	ctx context.Context,
	admin *model.Admin,
	followers []*model.CommentFollower,
	workspace *model.Workspace,
	projectID int,
	threadIDs []int,
	textForEmail string,
	viewLink string,
	muteLink string,
	sessionImage *string,
	action string,
	subjectScope string,
	asmGroupId *int,
) {
	var tos []*mail.Email
	var ccs []*mail.Email
	if admin.Email != nil {
		ccs = append(ccs, &mail.Email{Name: *admin.Name, Address: *admin.Email})
	}

	for _, f := range followers {
		if f.HasMuted != nil && *f.HasMuted {
			// remove the author's cc if they have unsubscribed from the thread
			if f.AdminId == admin.ID {
				ccs = nil
			}
			continue
		}

		// don't notify if the follower slack user is the reply author
		found := false
		for _, namePart := range strings.Split(*admin.Name, " ") {
			if strings.Contains(strings.ToLower(f.SlackChannelName), strings.ToLower(namePart)) {
				found = true
				break
			}
		}
		if found {
			continue
		}
		if f.AdminId > 0 {
			a := &model.Admin{}
			if err := r.DB.Where(&model.Admin{Model: model.Model{ID: f.AdminId}}).Take(&a).Error; err != nil {
				log.WithContext(ctx).Error(err, "Error finding follower admin object")
				continue
			}
			if a.Email != nil {
				if a.Name != nil {
					tos = append(tos, &mail.Email{Name: *a.Name, Address: *a.Email})
				} else {
					tos = append(tos, &mail.Email{Address: *a.Email})
				}

			}
		}
	}

	if len(threadIDs) > 0 {
		r.PrivateWorkerPool.SubmitRecover(func() {
			ctx := context.Background()
			commentMentionSlackSpan, _ := util.StartSpanFromContext(ctx, "resolver.sendFollowedCommentNotification",
				util.ResourceName("slackBot.sendCommentFollowerUpdate"), util.Tag("project_id", projectID), util.Tag("count", len(followers)), util.Tag("subjectScope", subjectScope))
			defer commentMentionSlackSpan.Finish()

			err := r.SendSlackThreadReply(ctx, workspace, admin, viewLink, textForEmail, action, subjectScope, threadIDs)
			if err != nil {
				log.WithContext(ctx).Error(e.Wrap(err, "error notifying tagged admins in comment for slack bot"))
			}
		})
	}

	if len(tos) > 0 {
		r.PrivateWorkerPool.SubmitRecover(func() {
			ctx := context.Background()
			commentMentionEmailSpan, _ := util.StartSpanFromContext(ctx, "resolver.sendFollowedCommentNotification",
				util.ResourceName("sendgrid.sendFollowerEmail"), util.Tag("project_id", projectID), util.Tag("count", len(followers)), util.Tag("action", action), util.Tag("subjectScope", subjectScope))
			defer commentMentionEmailSpan.Finish()

			err := r.SendEmailAlert(
				tos,
				ccs,
				*admin.Name,
				viewLink,
				muteLink,
				subjectScope,
				textForEmail,
				Email.SendGridCommentEmailTemplateID,
				sessionImage,
				asmGroupId,
			)
			if err != nil {
				log.WithContext(ctx).Error(e.Wrap(err, "error notifying tagged admins in comment"))
			}
		})
	}
}

func (r *Resolver) sendCommentMentionNotification(ctx context.Context, admin *model.Admin, taggedSlackUsers []*modelInputs.SanitizedSlackChannelInput, workspace *model.Workspace, projectID int, sessionCommentID *int, errorCommentID *int, textForEmail string, viewLink string, sessionImage *string, action string, subjectScope string, additionalContext *string) {
	r.PrivateWorkerPool.SubmitRecover(func() {
		ctx := context.Background()
		commentMentionSlackSpan, _ := util.StartSpanFromContext(ctx, "resolver.sendCommentMentionNotification",
			util.ResourceName("slackBot.sendCommentMention"), util.Tag("project_id", projectID), util.Tag("count", len(taggedSlackUsers)), util.Tag("subjectScope", subjectScope))
		defer commentMentionSlackSpan.Finish()

		err := r.SendSlackAlertToUser(ctx, workspace, admin, taggedSlackUsers, viewLink, textForEmail, action, subjectScope, sessionImage, sessionCommentID, errorCommentID, additionalContext)
		if err != nil {
			log.WithContext(ctx).Error(e.Wrap(err, "error notifying tagged admins in comment for slack bot"))
		}
	})
}

func (r *Resolver) sendCommentPrimaryNotification(
	ctx context.Context,
	admin *model.Admin,
	authorName string,
	taggedAdmins []*modelInputs.SanitizedAdminInput,
	workspace *model.Workspace,
	projectID int,
	sessionCommentID *int,
	errorCommentID *int,
	textForEmail string,
	viewLink string,
	muteLink string,
	sessionImage *string,
	action string,
	subjectScope string,
	additionalContext *string,
	asmGroupId *int,
) {
	var tos []*mail.Email
	var ccs []*mail.Email
	var adminIds []int

	if admin.Email != nil {
		if admin.Name != nil {
			ccs = append(ccs, &mail.Email{Name: *admin.Name, Address: *admin.Email})
		} else {
			ccs = append(ccs, &mail.Email{Address: *admin.Email})

		}
	}

	for _, taggedAdmin := range taggedAdmins {
		adminIds = append(adminIds, taggedAdmin.ID)

		if admin.Email != nil && taggedAdmin.Email == *admin.Email {
			if len(taggedAdmins) == 1 {
				ccs = nil
			} else {
				continue
			}
		}
		if taggedAdmin.Name != nil {
			tos = append(tos, &mail.Email{Name: *taggedAdmin.Name, Address: taggedAdmin.Email})
		} else {
			tos = append(tos, &mail.Email{Address: taggedAdmin.Email})
		}
	}

	r.PrivateWorkerPool.SubmitRecover(func() {
		ctx := context.Background()
		commentMentionEmailSpan, _ := util.StartSpanFromContext(ctx, "resolver.sendCommentPrimaryNotification",
			util.ResourceName("sendgrid.sendCommentMention"), util.Tag("project_id", projectID), util.Tag("count", len(taggedAdmins)), util.Tag("action", action), util.Tag("subjectScope", subjectScope))
		defer commentMentionEmailSpan.Finish()

		err := r.SendEmailAlert(
			tos,
			ccs,
			authorName,
			viewLink,
			muteLink,
			subjectScope,
			textForEmail,
			Email.SendGridCommentEmailTemplateID,
			sessionImage,
			asmGroupId,
		)
		if err != nil {
			log.WithContext(ctx).Error(e.Wrap(err, "error notifying tagged admins in comment"))
		}
	})

	r.PrivateWorkerPool.SubmitRecover(func() {
		ctx := context.Background()
		commentMentionSlackSpan, _ := util.StartSpanFromContext(ctx, "resolver.sendCommentPrimaryNotification",
			util.ResourceName("slack.sendCommentMention"), util.Tag("project_id", projectID), util.Tag("count", len(adminIds)), util.Tag("action", action), util.Tag("subjectScope", subjectScope))
		defer commentMentionSlackSpan.Finish()

		var admins []*model.Admin
		if err := r.DB.Find(&admins, adminIds).Where("slack_im_channel_id IS NOT NULL").Error; err != nil {
			log.WithContext(ctx).Error(e.Wrap(err, "error fetching admins"))
		}
		// return early if no admins w/ slack_im_channel_id
		if len(admins) < 1 {
			return
		}

		var taggedAdminSlacks []*modelInputs.SanitizedSlackChannelInput
		for _, a := range admins {
			taggedAdminSlacks = append(taggedAdminSlacks, &modelInputs.SanitizedSlackChannelInput{
				WebhookChannelID: a.SlackIMChannelID,
			})

			// See HIG-3438
			// We need additional debugging information to see if we can remove this code path.
			log.WithContext(ctx).WithFields(log.Fields{
				"admin_id": a.ID,
			}).Info("Sending slack notification for a Highlight user using a legacy slack configuration")
		}

		err := r.SendSlackAlertToUser(ctx, workspace, admin, taggedAdminSlacks, viewLink, textForEmail, action, subjectScope, nil, sessionCommentID, errorCommentID, additionalContext)
		if err != nil {
			log.WithContext(ctx).Error(e.Wrap(err, "error notifying tagged admins in comment"))
		}
	})

}

func (r *Resolver) IsInviteLinkExpired(inviteLink *model.WorkspaceInviteLink) bool {
	if inviteLink == nil {
		return true
	}
	// Links without an ExpirationDate never expire.
	if inviteLink.ExpirationDate == nil {
		return false
	}
	return time.Now().UTC().After(*inviteLink.ExpirationDate)
}

func (r *Resolver) isBrotliAccepted(ctx context.Context) bool {
	acceptEncodingString := ctx.Value(model.ContextKeys.AcceptEncoding).(string)
	return strings.Contains(acceptEncodingString, "br")
}

func (r *Resolver) getEvents(ctx context.Context, s *model.Session, cursor model.EventsCursor) ([]interface{}, error, *model.EventsCursor) {
	isLive := cursor != model.EventsCursor{}
	s3Events := map[int]string{}
	if !isLive {
		var err error
		s3Events, err = r.StorageClient.GetRawData(ctx, s.ID, s.ProjectID, model.PayloadTypeEvents)
		if err != nil {
			return nil, errors.Wrap(err, "error retrieving events objects from S3"), nil
		}
	}
	return r.Redis.GetEvents(ctx, s, cursor, s3Events)
}

func (r *Resolver) GetSlackChannelsFromSlack(ctx context.Context, workspaceId int) (*[]model.SlackChannel, int, error) {
	type result struct {
		ExistingChannels []model.SlackChannel
		NewChannelsCount int
	}
	res, err := redis.CachedEval(ctx, r.Redis, fmt.Sprintf(`slack-channels-workspace-%d`, workspaceId), 5*time.Second, 5*time.Minute, func() (*result, error) {
		workspace, _ := r.GetWorkspace(workspaceId)
		// workspace is not integrated with slack
		if workspace.SlackAccessToken == nil {
			return nil, nil
		}

		slackClient := slack.New(*workspace.SlackAccessToken)
		existingChannels, _ := workspace.IntegratedSlackChannels()

		getConversationsParam := slack.GetConversationsParameters{
			Limit: 1000,
			// public_channel is for public channels in the Slack workspace
			// private is for private channels in the Slack workspace that the Bot is included in
			// mpim is for multi-person conversations in the Slack workspace that the Bot is included in
			Types: []string{"public_channel", "private_channel", "mpim"},
		}
		allSlackChannelsFromAPI := []slack.Channel{}

		// Slack paginates the channels/people listing.
		for {
			channels, cursor, err := slackClient.GetConversations(&getConversationsParam)
			getConversationsParam.Cursor = cursor
			if err != nil {
				return nil, e.Wrap(err, "error getting Slack channels from Slack.")
			}

			allSlackChannelsFromAPI = append(allSlackChannelsFromAPI, channels...)

			if getConversationsParam.Cursor == "" {
				break
			}
			// delay the next slack call to avoid getting rate limited
			time.Sleep(time.Second)
		}

		// We need to get the users in the Slack channel in order to get their name.
		// The conversations endpoint only returns the user's ID, we'll use the response from `GetUsers` to get the name.
		users, err := slackClient.GetUsers()
		if err != nil {
			log.WithContext(ctx).Error(e.Wrap(err, "failed to get users"))
		}

		newChannelsCount := 0

		channelsAndUsers := map[string]model.SlackChannel{}
		for _, channel := range existingChannels {
			channelsAndUsers[channel.WebhookChannelID] = channel
		}

		for _, channel := range allSlackChannelsFromAPI {
			_, exists := channelsAndUsers[channel.ID]
			if !exists && channel.IsChannel && channel.ID != "" {
				newChannelsCount++
				slackChannel := model.SlackChannel{WebhookChannelID: channel.ID, WebhookChannel: fmt.Sprintf("#%s", channel.Name)}
				channelsAndUsers[channel.ID] = slackChannel
				existingChannels = append(existingChannels, slackChannel)
			}
		}

		for _, user := range users {
			_, exists := channelsAndUsers[user.ID]
			if !exists && !user.IsBot && !user.Deleted && strings.ToLower(user.Name) != "slackbot" {
				newChannelsCount++
				slackChannel := model.SlackChannel{WebhookChannelID: user.ID, WebhookChannel: fmt.Sprintf("@%s", user.Name)}
				channelsAndUsers[user.ID] = slackChannel
				existingChannels = append(existingChannels, slackChannel)
			}
		}

		return &result{existingChannels, newChannelsCount}, nil
	})
	if res == nil {
		return nil, 0, err
	}
	return &res.ExistingChannels, res.NewChannelsCount, err
}
func (r *Resolver) CreateSlackChannel(workspaceId int, name string) (*model.SlackChannel, error) {
	workspace, _ := r.GetWorkspace(workspaceId)
	// workspace is not integrated with slack
	if workspace.SlackAccessToken == nil {
		return nil, e.New("no slack access token provided")
	}

	slackClient := slack.New(*workspace.SlackAccessToken)
	channel, err := slackClient.CreateConversation(name, false)
	if err != nil {
		return nil, err
	}

	return &model.SlackChannel{
		WebhookChannel:   channel.Name,
		WebhookChannelID: channel.ID,
	}, nil
}

func (r *Resolver) UpsertDiscordChannel(workspaceId int, name string) (*model.DiscordChannel, error) {
	workspace, err := r.GetWorkspace(workspaceId)
	if err != nil {
		return nil, err
	}

	guildId := workspace.DiscordGuildId
	if guildId == nil {
		return nil, nil
	}

	bot, err := discord.NewDiscordBot(*guildId)
	if err != nil {
		return nil, err
	}

	channels, err := bot.GetChannels()
	if err != nil {
		return nil, err
	}

	if channel, has := lo.Find(channels, func(item *discordgo.Channel) bool {
		return strings.EqualFold(item.Name, name)
	}); has {
		return &model.DiscordChannel{
			Name: channel.Name,
			ID:   channel.ID,
		}, nil
	}

	channel, err := bot.CreateChannel(name)
	if err != nil {
		return nil, err
	}

	return &model.DiscordChannel{
		Name: channel.Name,
		ID:   channel.ID,
	}, nil
}

func GetAggregateFluxStatement(ctx context.Context, aggregator modelInputs.MetricAggregator, resMins int) string {
	fn := "mean"
	quantile := 0.
	// explicitly validate the aggregate func to ensure no query injection possible
	switch aggregator {
	case modelInputs.MetricAggregatorP50:
		quantile = 0.5
	case modelInputs.MetricAggregatorP75:
		quantile = 0.75
	case modelInputs.MetricAggregatorP90:
		quantile = 0.9
	case modelInputs.MetricAggregatorP95:
		quantile = 0.95
	case modelInputs.MetricAggregatorP99:
		quantile = 0.99
	case modelInputs.MetricAggregatorMax:
		quantile = 1.0
	case modelInputs.MetricAggregatorCount:
		fn = "count"
	case modelInputs.MetricAggregatorSum:
		fn = "sum"
	case modelInputs.MetricAggregatorAvg:
	default:
		log.WithContext(ctx).Errorf("Received an unsupported aggregateFunctionName: %+v", aggregator)
	}
	aggregateStatement := fmt.Sprintf(`
      query()
		  |> aggregateWindow(every: %dm, fn: %s, createEmpty: true)
          |> yield(name: "avg")
	`, resMins, fn)
	if quantile > 0. {
		aggregateStatement = fmt.Sprintf(`
		  do(q:%f)
			  |> yield(name: "p%d")
		`, quantile, int(quantile*100))
	}

	return aggregateStatement
}

func CalculateMetricUnitConversion(originalUnits *string, desiredUnits *string) float64 {
	div := 1.0
	if originalUnits == nil {
		originalUnits = pointy.String("s")
	}
	if desiredUnits == nil {
		return div
	}
	if *originalUnits == "b" {
		bytes, ok := BytesConversion[*desiredUnits]
		if !ok {
			return div
		}
		return float64(bytes)
	}
	o, err := time.ParseDuration(fmt.Sprintf(`1%s`, *originalUnits))
	if err != nil {
		return div
	}
	d, err := time.ParseDuration(fmt.Sprintf(`1%s`, *desiredUnits))
	if err != nil {
		return div
	}
	return float64(d.Nanoseconds()) / float64(o.Nanoseconds())
}

// MetricOriginalUnits returns the input units for the metric or nil if unitless.
func MetricOriginalUnits(metricName string) (originalUnits *string) {
	if strings.HasSuffix(metricName, "-ms") {
		originalUnits = pointy.String("ms")
	} else if map[string]bool{"fcp": true, "fid": true, "lcp": true, "ttfb": true, "jank": true, SessionActiveMetricName: true}[strings.ToLower(metricName)] {
		originalUnits = pointy.String("ms")
	} else if map[string]bool{"latency": true}[strings.ToLower(metricName)] {
		originalUnits = pointy.String("ns")
	} else if map[string]bool{"body_size": true, "response_size": true}[strings.ToLower(metricName)] {
		originalUnits = pointy.String("b")
	}
	return
}

// GetTagFilters returns the influxdb filter for a particular set of tag filters
func GetTagFilters(ctx context.Context, filters []*modelInputs.MetricTagFilterInput) (result string) {
	for _, f := range filters {
		if f != nil {
			var op, val string
			if f.Op != "" {
				switch f.Op {
				case modelInputs.MetricTagFilterOpEquals:
					op = "=="
					val = fmt.Sprintf(`"%s"`, f.Value)
				case modelInputs.MetricTagFilterOpContains:
					op = "=~"
					val = fmt.Sprintf("/.*%s.*/", f.Value)
				default:
					log.WithContext(ctx).Errorf("received an unsupported tag operator: %+v", f.Op)
				}
			}
			result += fmt.Sprintf(`|> filter(fn: (r) => r["%s"] %s %s)`, f.Tag, op, val) + "\n"
		}
	}
	return
}

// GetTagGroups returns the influxdb group columns for a particular set of tag groups
func GetTagGroups(groups []string) (result string) {
	result += "["
	for _, g := range groups {
		result += fmt.Sprintf(`"%s",`, g)
	}
	result += "]"
	return result
}

func GetMetricTimeline(ctx context.Context, tdb timeseries.DB, projectID int, metricName string, params modelInputs.DashboardParamsInput) (payload []*modelInputs.DashboardPayload, err error) {
	div := CalculateMetricUnitConversion(MetricOriginalUnits(metricName), params.Units)
	tagFilters := GetTagFilters(ctx, params.Filters)
	tagGroups := GetTagGroups(params.Groups)
	resMins := 60
	if params.ResolutionMinutes != nil && *params.ResolutionMinutes != 0 {
		resMins = *params.ResolutionMinutes
	}

	bucket, measurement := tdb.GetSampledMeasurement(tdb.GetBucket(strconv.Itoa(projectID), timeseries.Metrics), timeseries.Metrics, params.DateRange.EndDate.Sub(*params.DateRange.StartDate))
	query := fmt.Sprintf(`
      query = () =>
		from(bucket: "%[1]s")
		  |> range(start: %[2]s, stop: %[3]s)
		  |> filter(fn: (r) => r["_measurement"] == "%[4]s")
		  |> filter(fn: (r) => r["_field"] == "%[5]s")
		  %[6]s|> group(columns: %[8]s)
      do = (q) =>
        query()
		  |> aggregateWindow(
               every: %[7]dm,
               fn: (column, tables=<-) => tables |> quantile(q:q, column: column),
               createEmpty: true)
	`, bucket, params.DateRange.StartDate.Format(time.RFC3339), params.DateRange.EndDate.Format(time.RFC3339), measurement, metricName, tagFilters, resMins, tagGroups)
	agg := modelInputs.MetricAggregatorAvg
	if params.Aggregator != nil {
		agg = *params.Aggregator
	}
	query += GetAggregateFluxStatement(ctx, agg, resMins)
	timelineQuerySpan, _ := util.StartSpanFromContext(ctx, "tdb.queryTimeline")
	timelineQuerySpan.SetAttribute("projectID", projectID)
	timelineQuerySpan.SetAttribute("metricName", metricName)
	timelineQuerySpan.SetAttribute("resMins", resMins)
	results, err := tdb.Query(ctx, query)
	timelineQuerySpan.Finish()
	if err != nil {
		return nil, err
	}

	for _, r := range results {
		v := 0.
		if r.Value != nil {
			x, ok := r.Value.(float64)
			if !ok {
				v = float64(r.Value.(int64)) / div
			} else {
				v = x / div
			}
		}
		if len(params.Groups) > 0 {
			for _, g := range params.Groups {
				gVal := r.Values[g]
				if gVal == nil {
					continue
				}
				payload = append(payload, &modelInputs.DashboardPayload{
					Date:       r.Time.Format(time.RFC3339Nano),
					Value:      v,
					Aggregator: &agg,
					Group:      pointy.String(gVal.(string)),
				})
			}
		} else {
			payload = append(payload, &modelInputs.DashboardPayload{
				Date:       r.Time.Format(time.RFC3339Nano),
				Value:      v,
				Aggregator: &agg,
			})
		}
	}
	return
}

func (r *Resolver) GetProjectRetentionDate(projectId int) (time.Time, error) {
	var project *model.Project
	if err := r.DB.Model(&model.Project{}).Where("id = ?", projectId).Take(&project).Error; err != nil {
		return time.Time{}, e.Wrap(err, "error querying project")
	}

	workspace, err := r.GetWorkspace(project.WorkspaceID)
	if err != nil {
		return time.Time{}, err
	}
	return GetRetentionDate(workspace.RetentionPeriod), nil
}

func GetRetentionDate(retentionPeriodPtr *modelInputs.RetentionPeriod) time.Time {
	retentionPeriod := modelInputs.RetentionPeriodSixMonths
	if retentionPeriodPtr != nil {
		retentionPeriod = *retentionPeriodPtr
	}
	switch retentionPeriod {
	case modelInputs.RetentionPeriodThreeMonths:
		return time.Now().AddDate(0, -3, 0)
	case modelInputs.RetentionPeriodSixMonths:
		return time.Now().AddDate(0, -6, 0)
	case modelInputs.RetentionPeriodTwelveMonths:
		return time.Now().AddDate(-1, 0, 0)
	case modelInputs.RetentionPeriodTwoYears:
		return time.Now().AddDate(-2, 0, 0)
	}
	return time.Now()
}

func MergeHistogramBucketTimes(bucketTimes []time.Time, multiple int) []time.Time {
	newBucketTimes := []time.Time{}
	for i := 0; i < len(bucketTimes); i++ {
		// The last time is the end time of the search query and should not be removed
		if i%multiple == 0 || i == len(bucketTimes)-1 {
			newBucketTimes = append(newBucketTimes, bucketTimes[i])
		}
	}
	return newBucketTimes
}

func MergeHistogramBucketCounts(bucketCounts []int64, multiple int) []int64 {
	newBuckets := []int64{}
	newBucketsIndex := -1
	for i := 0; i < len(bucketCounts); i++ {
		if i%multiple == 0 {
			newBuckets = append(newBuckets, bucketCounts[i])
			newBucketsIndex++
		} else {
			newBuckets[newBucketsIndex] += bucketCounts[i]
		}
	}
	return newBuckets
}

func IsOptOutTokenValid(adminID int, token string) bool {
	if adminID <= 0 {
		return false
	}

	// If the token matches the current month's, it's valid
	if token == Email.GetOptOutToken(adminID, false) {
		return true
	}

	// If the token matches the prior month's, it's valid
	return token == Email.GetOptOutToken(adminID, true)
}

func (r *Resolver) CreateErrorTag(ctx context.Context, title string, description string) (*model.ErrorTag, error) {
	errorTag, err := r.EmbeddingsClient.GetErrorTagEmbedding(ctx, title, description)

	if err != nil {
		log.WithContext(ctx).Error(err, "CreateErrorTag: Error creating tag embedding")
		return nil, err
	}

	if err := r.DB.Create(errorTag).Error; err != nil {
		log.WithContext(ctx).Error(err, "CreateErrorTag: Error creating tag")
		return nil, err
	}

	return errorTag, nil
}

func (r *Resolver) GetErrorTags() ([]*model.ErrorTag, error) {
	var errorTags []*model.ErrorTag

	if err := r.DB.Model(errorTags).Scan(&errorTags).Error; err != nil {
		return nil, e.Wrap(err, "500: error querying error tags")
	}

	return errorTags, nil
}

func (r *Resolver) MatchErrorTag(ctx context.Context, query string) ([]*modelInputs.MatchedErrorTag, error) {
	stringEmbedding, err := r.EmbeddingsClient.GetStringEmbedding(ctx, query)

	if err != nil {
		return nil, e.Wrap(err, "500: failed to get string embedding")
	}

	var matchedErrorTags []*modelInputs.MatchedErrorTag
	if err := r.DB.Raw(`
		select error_tags.embedding <-> @string_embedding as score,
					error_tags.id as id,
					error_tags.title as title,
					error_tags.description as description
		from error_tags
		order by score
		limit 5;
	`, sql.Named("string_embedding", model.Vector(stringEmbedding))).
		Scan(&matchedErrorTags).Error; err != nil {
		return nil, e.Wrap(err, "error querying nearest ErrorTag")
	}

	return matchedErrorTags, nil
}

func (r *Resolver) FindSimilarErrors(ctx context.Context, query string) ([]*model.MatchedErrorObject, error) {
	stringEmbedding, err := r.EmbeddingsClient.GetStringEmbedding(ctx, query)

	if err != nil {
		return nil, e.Wrap(err, "500: failed to get string embedding")
	}

	var matchedErrorObjects []*model.MatchedErrorObject
	if err := r.DB.Raw(`
		select distinct on (1, error_group_id) eoep.gte_large_embedding <-> @string_embedding as score,
											   eo.*
		from error_object_embeddings_partitioned eoep
				 inner join error_objects eo on eoep.error_object_id = eo.id
		where eoep.gte_large_embedding is not null
		  and eoep.project_id = 1
		order by 1
		limit 10;
	`, sql.Named("string_embedding", model.Vector(stringEmbedding))).
		Scan(&matchedErrorObjects).Error; err != nil {
		return matchedErrorObjects, e.Wrap(err, "error querying nearest ErrorTag")
	}

	return matchedErrorObjects, nil
}
