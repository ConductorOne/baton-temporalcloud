package connector

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/conductorone/baton-sdk/pkg/types/entitlement"
	"github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"github.com/temporalio/tcld/protogen/api/auth/v1"
	"github.com/temporalio/tcld/protogen/api/authservice/v1"
	"go.uber.org/zap"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	cloudservicev1 "github.com/conductorone/baton-temporalcloud/pkg/pb/temporal/api/cloud/cloudservice/v1"
)

const (
	AccountPermissionAssignmentMaxWaitDuration = 10 * time.Minute
)

const (
	accountPermissionReadOnly  = "read"
	accountPermissionDeveloper = "developer"
	accountPermissionAdmin     = "admin"
)

var accountAccessLevels = []string{
	accountPermissionReadOnly,
	accountPermissionDeveloper,
	accountPermissionAdmin,
}

type accountBuilder struct {
	client            cloudservicev1.CloudServiceClient
	authServiceClient authservice.AuthServiceClient
}

func (o *accountBuilder) ResourceType(ctx context.Context) *v2.ResourceType {
	return accountResourceType
}

func (o *accountBuilder) List(ctx context.Context, parentResourceID *v2.ResourceId, pToken *pagination.Token) ([]*v2.Resource, string, annotations.Annotations, error) {
	accountID, err := o.getAccountID(ctx)
	if err != nil {
		return nil, "", nil, err
	}
	annos := &v2.V1Identifier{
		Id: fmt.Sprintf("account:%s", accountID),
	}

	account, err := resource.NewAppResource(accountID, accountResourceType, accountID, nil, resource.WithAnnotation(annos))
	if err != nil {
		return nil, "", nil, err
	}

	return []*v2.Resource{account}, "", nil, nil
}

var exp = regexp.MustCompile(`^(?:[a-z0-9\-]{2,39}\.)?([a-z0-9A-Z]{5})(?:-(?:admin|write|read|developer))$`)

func (o *accountBuilder) getAccountID(ctx context.Context) (string, error) {
	// Very janky way of getting the account ID but there is
	// not really a better option here
	resp, err := o.authServiceClient.GetUsers(ctx, &authservice.GetUsersRequest{PageSize: 1})
	if err != nil {
		return "", err
	}
	users := resp.GetUsers()
	if len(users) == 0 { // should never occur
		return "", errors.New("no users found for the account")
	}
	user := users[0]
	rolesResp, err := o.authServiceClient.GetRoles(ctx, &authservice.GetRolesRequest{UserId: user.GetId()})
	if err != nil {
		return "", err
	}
	roles := rolesResp.GetRoles()
	if len(roles) == 0 { // should also never occur
		return "", errors.New("no roles found")
	}
	role := roles[0]
	matches := exp.FindStringSubmatch(role.GetId())
	if matches == nil {
		return "", fmt.Errorf("could not parse account ID from role ID %s", role.GetId())
	}
	accountID := matches[1]
	return accountID, nil
}

func (o *accountBuilder) Entitlements(_ context.Context, r *v2.Resource, _ *pagination.Token) ([]*v2.Entitlement, string, annotations.Annotations, error) {
	rv := make([]*v2.Entitlement, 0, len(accountAccessLevels))
	for _, level := range accountAccessLevels {
		annos := &v2.V1Identifier{
			Id: fmt.Sprintf("account:%s:role%s", r.Id.Resource, level),
		}

		e := entitlement.NewPermissionEntitlement(
			r,
			level,
			entitlement.WithDisplayName(fmt.Sprintf("Account %s %s", r.GetDisplayName(), cases.Title(language.English).String(level))),
			entitlement.WithDescription(fmt.Sprintf("Access to account %s in Temporal Cloud", r.GetDisplayName())),
			entitlement.WithAnnotation(annos),
			entitlement.WithGrantableTo(userResourceType),
		)
		e.Resource.ParentResourceId = r.GetId()
		rv = append(rv, e)
	}

	return rv, "", nil, nil
}

func (o *accountBuilder) Grants(ctx context.Context, r *v2.Resource, pToken *pagination.Token) ([]*v2.Grant, string, annotations.Annotations, error) {
	resp, err := o.client.GetUsers(ctx, &cloudservicev1.GetUsersRequest{})
	if err != nil {
		return nil, "", nil, err
	}

	var rv []*v2.Grant
	for _, user := range resp.GetUsers() {
		permission := user.GetSpec().GetAccess().GetAccountAccess().GetRole()

		g, err := createAccountGrant(user, r, permission)
		if err != nil {
			return nil, "", nil, err
		}
		rv = append(rv, g)
	}

	return rv, "", nil, nil
}

func (o *accountBuilder) Grant(ctx context.Context, principal *v2.Resource, e *v2.Entitlement) ([]*v2.Grant, annotations.Annotations, error) {
	accountRole := e.GetSlug()
	entitlementID := e.GetId()
	userID := principal.GetId().GetResource()
	userType := principal.GetId().GetResourceType()
	account := e.GetResource()
	accountID := account.GetId().GetResource()
	accountType := account.GetId().GetResourceType()

	switch r := strings.ToLower(accountRole); r {
	case strings.ToLower(auth.ACCOUNT_ACTION_GROUP_ADMIN.String()):
	case strings.ToLower(auth.ACCOUNT_ACTION_GROUP_DEVELOPER.String()):
	case strings.ToLower(auth.ACCOUNT_ACTION_GROUP_READ.String()):
	default:
		return nil, nil, fmt.Errorf("temporalcloud-connnector: unrecognized account entitlement ID: %s", accountRole)
	}
	userResp, err := o.client.GetUser(ctx, &cloudservicev1.GetUserRequest{UserId: userID})
	if err != nil {
		return nil, nil, fmt.Errorf("temporalcloud-connnector: couldn't retrieve user: %w", err)
	}
	user := userResp.GetUser()
	spec := user.GetSpec()
	spec.Access.AccountAccess.Role = accountRole

	req := &cloudservicev1.UpdateUserRequest{UserId: userID, Spec: spec, ResourceVersion: user.GetResourceVersion()}
	resp, err := o.client.UpdateUser(ctx, req)
	if err != nil {
		return nil, nil, fmt.Errorf("temporalcloud-connector: could not grant entitlement to user: %w", err)
	}

	retryDelay := resp.GetAsyncOperation().GetCheckDuration().AsDuration()
	requestID := resp.GetAsyncOperation().GetId()
	l := ctxzap.Extract(ctx).With(
		zap.String("request_id", requestID),
		zap.String("principal_id", userID),
		zap.String("principal_type", userType),
		zap.String("entitlement_id", entitlementID),
		zap.String("entitlement_resource_id", accountID),
		zap.String("entitlement_resource_type", accountType),
	)
	waitCtx, cancel := context.WithTimeout(ctx, AccountPermissionAssignmentMaxWaitDuration)
	defer cancel()
	err = awaitAsyncOperation(waitCtx, l, o.client, requestID, retryDelay)
	if err != nil {
		return nil, nil, fmt.Errorf("temporalcloud-connector: account assignment creation failed: %w", err)
	}

	annos := annotations.New()
	annos.Append(&v2.RequestId{RequestId: requestID})

	g, err := createAccountGrant(user, account, accountRole)
	if err != nil {
		return nil, nil, err
	}

	return []*v2.Grant{g}, annos, nil
}

func (o *accountBuilder) Revoke(ctx context.Context, g *v2.Grant) (annotations.Annotations, error) {
	userID := g.GetPrincipal().GetId().GetResource()
	userType := g.GetPrincipal().GetId().GetResourceType()
	entitlementID := g.GetEntitlement().GetId()
	account := g.GetEntitlement().GetResource()
	accountID := account.GetId().GetResource()
	accountType := account.GetId().GetResourceType()

	userResp, err := o.client.GetUser(ctx, &cloudservicev1.GetUserRequest{UserId: userID})
	if err != nil {
		return nil, fmt.Errorf("temporalcloud-connector: couldn't retrieve user: %w", err)
	}

	resp, err := o.client.DeleteUser(ctx, &cloudservicev1.DeleteUserRequest{UserId: userID, ResourceVersion: userResp.GetUser().GetResourceVersion()})
	if err != nil {
		return nil, fmt.Errorf("temporalcloud-connector: grant does not exist for user")
	}

	retryDelay := resp.GetAsyncOperation().GetCheckDuration().AsDuration()
	requestID := resp.GetAsyncOperation().GetId()
	l := ctxzap.Extract(ctx).With(
		zap.String("request_id", requestID),
		zap.String("principal_id", userID),
		zap.String("principal_type", userType),
		zap.String("entitlement_id", entitlementID),
		zap.String("entitlement_resource_id", accountID),
		zap.String("entitlement_resource_type", accountType),
	)
	waitCtx, cancel := context.WithTimeout(ctx, AccountPermissionAssignmentMaxWaitDuration)
	defer cancel()
	err = awaitAsyncOperation(waitCtx, l, o.client, requestID, retryDelay)
	if err != nil {
		return nil, fmt.Errorf("temporalcloud-connector: account assignment deletion failed: %w", err)
	}

	annos := annotations.New()
	annos.Append(&v2.RequestId{RequestId: requestID})

	return annos, nil
}

func newAccountBuilder(client cloudservicev1.CloudServiceClient, authClient authservice.AuthServiceClient) *accountBuilder {
	return &accountBuilder{
		client:            client,
		authServiceClient: authClient,
	}
}
